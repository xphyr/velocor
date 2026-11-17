# Upstreaming the SPDK WRITE SAME fix

Status: carried locally as
[`deploy/containers/spdk-patches/0001-scsi-bound-WRITE-SAME-fan-out.patch`](../../deploy/containers/spdk-patches/0001-scsi-bound-WRITE-SAME-fan-out.patch)
(Velocor commit 559ea38, tracked as TODO P1-10). This guide covers submitting it to SPDK, plus
a GitHub issue for the deeper bdev-layer starvation it works around. Once SPDK merges it,
remove the patch file (see [After it merges](#7-after-it-merges)).

## 1. What the patch fixes

The problem is in `lib/scsi/scsi_bdev.c`, which is unchanged on SPDK `master` as of 2026-11-17.

1. **Unbounded fan-out.** SPDK runs a SCSI WRITE SAME as one single-block bdev write per block,
   and `bdev_scsi_split()` keeps submitting children until the bdev_io pool returns `-ENOMEM`.
   - The Block Limits VPD advertises MAXIMUM WRITE SAME LENGTH = 4 MiB / block size, which is
     8192 blocks at 512 B. So one command becomes 8192 bdev I/Os.
   - Linux zeroing (BLKZEROOUT, `mkfs.ext4` over existing data) sends as many of these as the
     queue depth allows (32).
2. **Deadlock on thin lvols.** After a whole-device UNMAP, every one of those writes needs a
   cluster allocation.
   - The allocation's metadata write runs on the blobstore md thread.
   - The flood empties the global bdev_io pool, so the md thread's request goes to *its own*
     `io_wait_queue`.
   - That queue is only woken by a bdev_io freed on that thread (`spdk_bdev_free_io()` in
     `lib/bdev/bdev.c`). Nothing is in flight there, so it never wakes up.
   - The result: the LUN hangs, the iSCSI poll group spins at about 85% CPU, and nothing is
     logged. ABORT TASK isn't supported and LUN RESET doesn't clear it. `spdk_tgt` can't shut
     down and needs SIGKILL.
3. **Lost completions.** The return values of `bdev_scsi_split()` (from the `-ENOMEM`
   resubmission and from the child-completion path) and of `bdev_scsi_process_block()` (from
   `bdev_scsi_process_block_resubmit()`) are ignored. If a split ends there with an error, for
   example when a child fails to submit after a batch, the SCSI task is never completed.

What the patch does:

- An all-zero WRITE SAME without LBDATA/PBDATA becomes one `spdk_bdev_write_zeroes_blocks()`. The
  bdev layer emulates it where the backend doesn't support it. The UNMAP bit permits
  deallocation but doesn't require it, so writing zeroes still satisfies it.
- Split commands (UNMAP, non-zero WRITE SAME) keep at most `SCSI_SPLIT_MAX_OUTSTANDING` (16)
  children in flight.
- All three call sites now complete the task when the call returns `SPDK_SCSI_TASK_COMPLETE`.

Unit tests are added to `test/unit/lib/scsi/scsi_bdev.c/scsi_bdev_ut.c`:

| Test | What it checks |
|---|---|
| `write_same_zero_test` | Zero fill becomes one WRITE ZEROES; under `-ENOMEM` it resumes from io_wait; LBDATA still splits |
| `write_same_split_cap_test` | No more than 16 children in flight; all 40 blocks written |
| `write_same_split_error_test` | A child that fails to submit after a pool-limited batch still completes the task with CHECK CONDITION |

All three fail on unpatched SPDK v26.09. With the patch, every SCSI unit test passes, and
`scsi_bdev.c` builds without warnings.

## 2. Evidence to quote (lab, SPDK v26.09 `0bbb7fe4`)

Setup:

- **Target:** `spdk_tgt -m 0x1 -s 1024`; raid1 of two `bdev_uring` disks (512 B blocks); lvstore
  with 1 MiB clusters; a **thin** 3 GiB lvol exported as an iSCSI LUN.
- **Initiator:** Fedora 44, open-iscsi, `sg3_utils`.

After `blkdiscard` of the whole LUN, N parallel `sg_write_same --10 --num=8192` commands, each to
its own 4 MiB range:

| N | Unpatched | Patched |
|---|---|---|
| 12 | 0.8 s | |
| 16 | 1.0 s | 0.2 s |
| 24 | **hang** (no completion; abort + LU reset fail) | 0.3 s |
| 32 | | 0.3 s |
| 64 | | 0.6 s |

On **allocated** clusters, unpatched SPDK completes N=24 in 1.4 s and N=32 in 2.0 s. That
isolates the problem to the cluster-allocation path.

While the LUN was hung on unpatched SPDK:

- `bdev_get_iostat`: lvol, raid and base write counts frozen. About 71k of the 196k child writes
  had completed.
- `thread_get_stats`: the iSCSI poll group was ~85% busy, with 0 writes per 5 s.
- At the next I/O channel creation, the log said `bdev_mgmt_channel_create: You need to increase
  bdev_io_pool_size using bdev_set_options RPC`. The pool size was the default 65535.

With the patch:

- A zero WRITE SAME over random data reads back zero, and neighbouring blocks are unchanged.
- A patterned WRITE SAME of 40 and 3000 blocks writes the exact pattern.
- A whole-device BLKZEROOUT reads back zero.
- `mkfs.ext4` over existing data (twice): fsck is clean, and write/remount/verify passes.

## 3. Reproducing it

On the initiator, against the SPDK iSCSI LUN of a thin lvol:

```sh
D=/dev/sdX            # the SPDK LUN; everything on it is destroyed
for N in 16 24 32; do
  blkdiscard "$D"
  for i in $(seq 0 $((N-1))); do
    timeout 60 sg_write_same --10 --lba=$((i*8192)) --num=8192 "$D" &
  done
  fails=0; for j in $(jobs -p); do wait "$j" || fails=$((fails+1)); done
  echo "N=$N failed=$fails"
  [ "$fails" -gt 0 ] && break
done
```

Or the real-world trigger: `mkfs.ext4 -F $D` twice in a row. The second run discards and then
zeroes with WRITE SAME.

After a hang:

1. Log the initiator out first. Otherwise it resends the same commands to a restarted target
   and wedges it again, which looks like persistent damage but isn't.
2. Restart `spdk_tgt` (it will need SIGKILL).
3. The volume reads back fine afterwards.

## 4. One-time setup

1. Create an account at <https://review.spdk.io> (Gerrit; SPDK doesn't take GitHub pull
   requests) and add your SSH key or HTTP password there.
2. Clone through Gerrit, with submodules, and install the Change-Id hook:
   ```sh
   git clone https://review.spdk.io/spdk/spdk && cd spdk
   git submodule update --init
   # Gerrit's commit-msg hook (adds the required Change-Id line):
   curl -Lo .git/hooks/commit-msg https://review.spdk.io/tools/hooks/commit-msg
   chmod +x .git/hooks/commit-msg
   git config core.hooksPath .githooks   # SPDK's own format pre-commit/pre-push hooks, optional
   ```
   Check <https://spdk.io/development/> for the current push remote. The page shows
   `git remote set-url origin https://review.spdk.io/spdk/spdk` and `git push review`.
3. Install `astyle` and the Python lint tools that `scripts/check_format.sh` wants. On Fedora,
   `scripts/pkgdep.sh` installs the build dependencies.

## 5. Preparing the change

1. Branch from current `master`, not v26.09:
   `git checkout -b scsi-write-same origin/master`.
2. Apply the patch. It's a plain `diff -u`, not a `git format-patch` mail, so use `git apply`:
   ```sh
   git apply --check /path/to/velocor/deploy/containers/spdk-patches/0001-scsi-bound-WRITE-SAME-fan-out.patch
   git apply /path/to/velocor/deploy/containers/spdk-patches/0001-scsi-bound-WRITE-SAME-fan-out.patch
   ```
   If `master` has moved and the patch no longer applies, port it by hand; it's small.
3. Recommended: split it into three commits. SPDK reviewers prefer one purpose per change, and
   each part stands on its own with its own test. Use `git add -p`:

   | # | Subject | Source hunks | Test hunks |
   |---|---|---|---|
   | 1 | `scsi: complete tasks whose split or resubmission finishes synchronously` | `bdev_scsi_split_resubmit()`, the end of `bdev_scsi_task_complete_split_cmd()`, `bdev_scsi_process_block_resubmit()` | `g_writev_count` / `g_writev_fail_at` in the `spdk_bdev_writev_blocks` mock, `ut_write_same_task` / `ut_write_same_reset`, `write_same_split_error_test` |
   | 2 | `scsi: limit outstanding child I/Os of split commands` | `SCSI_SPLIT_MAX_OUTSTANDING` and the `while` condition in `bdev_scsi_split()` | `write_same_split_cap_test` |
   | 3 | `scsi: issue zero-filled WRITE SAME as WRITE ZEROES` | the LBDATA/PBDATA defines, `bdev_scsi_task_data_is_zero()`, the fast path in `bdev_scsi_write_same()` | the `spdk_bdev_write_zeroes_blocks` mock + `g_write_zeroes_count`, `write_same_zero_test` |

   Build and run the tests after **each** commit. Patch 1's test uses a pool limit of 16, so it
   doesn't depend on patch 2.

   Submitting it as a single change is also acceptable; reviewers may then ask for the split.
4. Run the format check: `./scripts/check_format.sh`. It runs astyle, and SPDK wraps at 100
   columns. The `if (!(flags & (SCSI_WRITE_SAME_PBDATA | ...)) && ...)` line is over that and
   will need wrapping. Commit any changes astyle makes.
5. Build with unit tests and run them:
   ```sh
   ./configure --with-uring && make -j"$(nproc)"
   ./test/unit/lib/scsi/scsi_bdev.c/scsi_bdev_ut
   ./test/unit/unittest.sh          # the full unit suite, as CI runs it
   ```
6. Commit each change signed off (DCO), with a subject in the form `component: description`:
   `git commit -s`. Message for change 3, or for a single combined change (trim it to fit each
   commit of a split):

   ```
   scsi: issue zero-filled WRITE SAME as WRITE ZEROES

   WRITE SAME is executed as one single-block write per block, submitted
   until the bdev_io pool returns -ENOMEM. A 4 MiB WRITE SAME on a 512-byte
   LUN is 8192 bdev I/Os, and Linux zeroing (BLKZEROOUT, mkfs) sends up to
   the queue depth of them. On a thin lvol after a whole-device UNMAP this
   empties the bdev_io pool while the cluster allocations' metadata writes
   wait on the blobstore md thread, whose io_wait entry is only woken by a
   bdev_io freed on that thread: the LUN hangs with nothing logged.

   An all-zero WRITE SAME without LBDATA/PBDATA is now issued as a single
   spdk_bdev_write_zeroes_blocks(), emulated by the bdev layer where the
   bdev doesn't support it. The UNMAP bit allows but doesn't require
   deallocation, so writing zeroes satisfies it.

   Fixes #<GitHub issue number>

   Signed-off-by: Your Name <you@example.com>
   ```
   Gerrit's hook adds the `Change-Id:` line. File the GitHub issue (section 8) before
   committing, so the message can say `Fixes #N`.

## 6. Submitting and following through

1. Push for review. The remote name depends on your setup; Gerrit's convention is:
   ```sh
   git push review HEAD:refs/for/master
   # work in progress (CI won't run): HEAD:refs/for/master%wip
   ```
   A series of three becomes three linked changes.
2. CI runs on every change and votes Verified +1/-1 with a link to its logs. If a failure is
   unrelated and intermittent, comment `false positive: <issue number>` with the matching
   "Intermittent Failure" issue.
3. Add reviewers. Look at `git log --format='%an' -- lib/scsi/scsi_bdev.c | sort | uniq -c |
   sort -rn | head` for recent maintainers of the file. Maintainers are also reachable on SPDK's
   Slack (see the Community page on spdk.io).
4. To address review comments: amend the same commit, keep its `Change-Id`, and push again. For
   a series, use `git rebase -i` and push again.
5. Expect discussion on:
   - the cap value (16) and whether it should be configurable;
   - whether zero detection should also cover WRITE SAME(16) NDOB (SPDK currently requires a
     one-block data-out);
   - whether the fix belongs in the bdev layer (section 8).

## 7. After it merges

1. Note which SPDK release contains it.
2. When Velocor moves to that release (update `SPDK_VERSION` / `SPDK_COMMIT` in
   `deploy/containers/Containerfile.spdk`), delete
   `deploy/containers/spdk-patches/0001-scsi-bound-WRITE-SAME-fan-out.patch`. The build fails if
   a patch no longer applies, so a stale patch is noticed at the bump.
3. Until then, CI keeps running the patch's tests (`make spdk-unit-tests`, the SPDK patch
   unit-test job). If review changes the patch (a different cap, reworked code), update the local
   patch to match what merged, so the appliance runs the reviewed version.
4. Update TODO P1-10 with the upstream link.

## 8. GitHub issue: the underlying bdev starvation

The SCSI change removes the trigger. The bdev-layer behaviour remains: a thread can wait forever
when the global pool is drained by I/O that depends on that thread. Report it separately at
<https://github.com/spdk/spdk/issues/new/choose>, as a bug. Draft:

> **Title:** bdev: io_wait on one thread never wakes when another thread's I/O holds the
> bdev_io pool (iSCSI WRITE SAME on thin lvol hangs)
>
> **Version:** v26.09 (0bbb7fe4), also `lib/scsi/scsi_bdev.c` on master.
>
> **What happens:** After a whole-device UNMAP of a thin lvol exported over iSCSI, about 24
> parallel 4 MiB WRITE SAME(10) commands (512 B blocks) hang the LUN permanently.
> - `bdev_scsi_split()` turns each command into 8192 single-block writes, submitted until
>   `-ENOMEM`.
> - Each write targets an unallocated cluster, so it queues for cluster allocation. The
>   allocation's metadata write runs on the blobstore md thread (app_thread).
> - The global bdev_io pool (65535) is exhausted by the iSCSI poll group, so the md thread's
>   I/O enters its own `io_wait_queue`. `spdk_bdev_free_io()` only wakes waiters on the
>   freeing thread's mgmt channel, and the md thread has nothing in flight, so it never wakes.
> - No error is logged. The iSCSI thread spins at ~85% with zero completions.
>   ABORT TASK is unsupported, and LU reset doesn't help.
>
> **Reproduce:** see the script above. N=16 completes; N=24 hangs; the same burst on
> allocated clusters completes at N=32.
>
> **Suggested fixes:**
> - (a) Bound the SCSI split fan-out and issue zero WRITE SAME as WRITE ZEROES. Change
>   <link>.
> - (b) In the bdev layer, let a waiter on a thread with no outstanding I/O be woken by frees
>   on other threads (or retry periodically), or reserve a small per-thread pool for metadata
>   I/O.
> - (c) Implement ABORT TASK / make LU reset fail stuck tasks.

Attach the before/after table from section 2.
