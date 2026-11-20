# Performance measurements

Measured results from lab runs, newest first. Each run says what it was measured on, so
numbers from different setups aren't compared by accident. These are lab numbers on virtual
disks, not a statement of what Velocor does on real hardware.

## Method

One raw block PVC (16 GiB) per StorageClass, written in full before the first job, and one fio
pod per class on the same node. Every job runs 60 s after a 10 s ramp, with `direct=1` and the
`libaio` engine. All jobs run against both classes in turn, three rounds, alternating which
class goes first. The tables give each job's median over the three rounds.

| Job | Pattern | Block size | Queue depth × jobs |
|---|---|---|---|
| `lat-randread` | random read | 4K | 1 × 1 |
| `lat-randwrite` | random write | 4K | 1 × 1 |
| `iops-randread` | random read | 4K | 16 × 4 |
| `iops-randwrite` | random write | 4K | 16 × 4 |
| `mixed-70-30` | random 70% read / 30% write | 4K | 16 × 4 |
| `seq-read` | sequential read | 128K | 16 × 1 |
| `seq-write` | sequential write | 128K | 16 × 1 |
| `dsync-randwrite-qd1` | random write, `O_DSYNC` | 4K | 1 × 1 |
| `dsync-randwrite` | random write, `O_DSYNC` | 4K | 16 × 4 |
| `dsync-seq-write` | sequential write, `O_DSYNC` | 128K | 16 × 1 |
| `fsync-randwrite-qd1` | random write, fsync after each write | 4K | 1 × 1 |

While the jobs ran, a sampler on the appliance recorded every 10 s: CPU time, per-disk I/O and
utilisation (`/proc/diskstats`), SAN NIC bytes, and each SPDK thread's busy/idle ticks
(`thread_get_stats`).

## 2026-11-20: SPDK against the kernel data path (three ways)

**Question.** Would a kernel storage stack (md RAID1, LVM thin, kernel nvmet-tcp) be in the same
performance class as SPDK's (RAID1 bdev, lvol, SPDK NVMe/TCP target) on this lab? And what
does the hybrid (kernel md + LVM underneath, SPDK's target on top through `bdev_uring` or
`bdev_aio`) cost?

**Setup.** The test appliance (4 vCPU, one SPDK reactor core, build dev-8311b3a) with two
identical pairs of 250 GB virtio-scsi disks on the same NAS: one pair as a Velocor RAID1 pool
(SPDK), the other as md RAID1 with a write-intent bitmap, an LVM thin pool (1 MiB chunks) and
a thick LV. Six 16 GiB volumes, each filled first: `spdk-thin`, `spdk-thick` (Velocor volumes
on the SPDK pair); `kernel-thin`, `kernel-thick` (LVs exported by kernel nvmet-tcp);
`hyb-uring`, `hyb-aio` (thin LVs exported by SPDK's target through `bdev_uring` / `bdev_aio`).
Client: a privileged fio pod on the Arm cluster node's host network over the 10 GbE SAN,
connecting every target the same way (`nvme connect`), so the client path is identical. The
usual job table (60 s after a 10 s ramp, `direct=1`, `libaio`), three rounds alternating the
target order, medians below. Fill rates (MB/s): spdk-thin 549, hyb-uring 536, hyb-aio 526, kernel-thin 547, spdk-thick 566, kernel-thick 502.

### IOPS / MB/s

| job | spdk-thin | hyb-uring | hyb-aio | kernel-thin | spdk-thick | kernel-thick |
|---|---|---|---|---|---|---|
| lat-randread | 1786 | 1693 | 1790 | 1738 | 1765 | 1806 |
| lat-randwrite | 3705 | 3178 | 3676 | 3732 | 3772 | 3931 |
| iops-randread | 44897 | 29202 | 25650 | 33232 | 44969 | 39460 |
| iops-randwrite | 29517 | 22239 | 21072 | 18055 | 28617 | 19712 |
| mixed-70-30 | 18801 / 8090 | 15695 / 6753 | 14789 / 6364 | 16277 / 7002 | 19688 / 8470 | 18497 / 7956 |
| seq-read | 1023 MB/s | 824 MB/s | 1004 MB/s | 751 MB/s | 1044 MB/s | 640 MB/s |
| seq-write | 535 MB/s | 505 MB/s | 556 MB/s | 470 MB/s | 572 MB/s | 483 MB/s |

### Latency, µs (mean / p99 / p99.9; reads for read and mixed jobs, writes for write jobs)

| job | spdk-thin | hyb-uring | hyb-aio | kernel-thin | spdk-thick | kernel-thick |
|---|---|---|---|---|---|---|
| lat-randread | 543 / 905 / 1827 | 573 / 1679 / 2310 | 541 / 913 / 1974 | 552 / 922 / 2245 | 549 / 946 / 2408 | 530 / 897 / 2540 |
| lat-randwrite | 245 / 627 / 2605 | 291 / 1597 / 3916 | 247 / 692 / 3785 | 245 / 618 / 4686 | 243 / 733 / 3228 | 230 / 618 / 3228 |
| iops-randread | 1417 / 4817 / 9241 | 2181 / 4358 / 8454 | 2486 / 4489 / 10158 | 1917 / 3981 / 8585 | 1413 / 4620 / 9503 | 1613 / 4145 / 9372 |
| iops-randwrite | 2158 / 12517 / 26870 | 2866 / 21103 / 50594 | 3026 / 20578 / 44827 | 3534 / 20578 / 44302 | 2224 / 12648 / 26345 | 3236 / 19005 / 39584 |
| mixed-70-30 | 2438 / 10420 / 22675 | 2678 / 11207 / 28967 | 3018 / 10945 / 37487 | 2941 / 11600 / 31850 | 2342 / 11076 / 23986 | 2586 / 11076 / 28180 |
| seq-read | 2021 / 4358 / 12386 | 2517 / 5407 / 7569 | 2058 / 3883 / 6783 | 2760 / 4293 / 72876 | 1979 / 3523 / 7963 | 3252 / 4686 / 522191 |
| seq-write | 3862 / 17695 / 38011 | 4102 / 21103 / 141558 | 3746 / 20840 / 112722 | 4412 / 20578 / 63701 | 3639 / 17957 / 36962 | 4319 / 20054 / 59507 |

### Appliance CPU, % of a vCPU (400 = all four): host total / SPDK threads busy

The idle appliance uses about 104%: SPDK's reactor polls at 100% of one vCPU whether or not it
has work, so "host total" for the kernel targets includes that idle spin. SPDK's own work is
its threads' busy share plus the kernel-side part of the host total above the baseline.

| job | spdk-thin | hyb-uring | hyb-aio | kernel-thin | spdk-thick | kernel-thick |
|---|---|---|---|---|---|---|
| lat-randread | 106 / 13 | 106 / 15 | 106 / 13 | 111 / 0 | 105 / 13 | 110 / 0 |
| lat-randwrite | 110 / 31 | 119 / 23 | 121 / 19 | 133 / 1 | 113 / 32 | 133 / 1 |
| iops-randread | 126 / 83 | 125 / 62 | 118 / 94 | 209 / 1 | 126 / 83 | 209 / 1 |
| iops-randwrite | 169 / 81 | 196 / 61 | 210 / 68 | 228 / 1 | 170 / 80 | 223 / 1 |
| mixed-70-30 | 138 / 76 | 161 / 63 | 161 / 95 | 217 / 1 | 141 / 80 | 214 / 1 |
| seq-read | 120 / 76 | 116 / 57 | 120 / 81 | 176 / 1 | 117 / 75 | 163 / 1 |
| seq-write | 155 / 86 | 170 / 76 | 173 / 74 | 210 / 1 | 157 / 86 | 209 / 1 |

### Where the kernel write gap comes from

Single 60 s runs through kernel nvmet-tcp over the same two disks, no LVM, no bitmap:

| device under nvmet | iops-randwrite | iops-randread | seq-read |
|---|---|---|---|
| one raw disk (no RAID) | 34,317 | 47,213 | 224 MB/s |
| md RAID1 | 21,286 | 43,599 | 409 MB/s |
| md RAID10 (2 disks, near=2) | 21,264 | 42,175 | 478 MB/s |
| kernel-thick with the bitmap removed | 19,940 | 39,118 | 186 MB/s |
| SPDK RAID1 (from the table above) | 29,517 | 44,897 | 1,023 MB/s |

### Reading the numbers

- **QD1 latency is the same for every stack**: ~540 µs random read, ~245 µs random write mean,
  with p99 within a few percent. The network and the virtual disks set the floor; the stack
  doesn't show. The one exception is `bdev_uring` over an LV, whose p99 is nearly double.
- **Deep random reads are even** (SPDK 45k, kernel thick 39k, bare md 43k); thin LVs cost
  about 15% (33k) where SPDK's thin lvol costs nothing measurable.
- **Deep random writes are the kernel stack's deficit**: ~20k against SPDK's ~29k. It is md's
  mirror write path, not nvmet (34k on a raw disk), not the bitmap (same without it), not LVM
  (bare md gives the same 21k) and not the RAID level (RAID10 is identical).
- **Sequential read is not a usable metric on this lab**: the same kernel device gave 186, 409
  and 640 MB/s in three runs, and a single raw disk 224 MB/s, so the NAS behind the virtual
  disks dominates it. SPDK's 1 GB/s (reading both mirror halves) stands, but md RAID1's
  single-disk sequential reads can't be judged from these runs.
- **CPU.** At depth SPDK does about 30% more IOPS per unit of CPU actually spent (deep reads:
  ~105% of a vCPU for 45k against ~105% for 33–39k; deep writes: ~146% for 29.5k against
  ~120% for 18–20k). Against that, SPDK reserves a whole vCPU, a quarter of this appliance,
  polling while idle; the kernel stack idles at a few percent.
- **The hybrids are the worst of both**: SPDK's reactor plus the kernel block layer under it,
  fewer IOPS than either pure stack, the highest tail latencies, and `bdev_uring` passes only
  reads, writes and write-zeroes (no FLUSH, no discard, checked in `bdev_get_bdevs`), so thin
  volumes behind it could never reclaim space. `bdev_aio` adds FLUSH but still no discard.

**Not measured here:** real NVMe drives, where SPDK's userspace driver and polling should show
an advantage neither side has on virtual disks; iSCSI (LIO against SPDK's target); and the
kernel stack at higher core counts.

## 2026-11-20: host I/O during a scrub

**Setup.** The appliance on virtio-scsi disks (one SPDK core), build dev-8311b3a; one 20 GiB raw
block iSCSI PVC on a RAID1 pool, filled first; 4K random 70% read / 30% write for 60 s with
`direct=1`, `libaio`, one job; a single run each, so differences of a few percent are noise.

| Scrub on the pool | QD1 read / write IOPS | QD1 read p99 | QD16 read / write IOPS |
|---|---|---|---|
| none | 1,075 / 458 | 1.5 ms | 11,236 / 4,819 |
| 200 MiB/s (the default limit) | 856 / 367 | 4.0 ms | 9,490 / 4,075 |
| unthrottled (~1.17 GB/s) | 443 / 189 | 3.7 ms | 5,225 / 2,243 |

- At the default limit a scrub costs about a sixth of the pool's IOPS; unthrottled it halves them.
- Tail latency at QD1 rises about as much either way: each 1 MiB window briefly holds host I/O
  to that range; the limit only spaces the windows out.
- The unthrottled row was taken before the rate-limit fix (dev-3705715), when the limit stopped
  working after ~7 GB.

## 2026-11-19: baseline before the next Fedora release (Fedora 45)

**Setup.** The same appliance and client as the virtio-scsi run below, on build dev-cdf1064
(Fedora 45, kernel 7.3.4; P0-4 write-cache handling, P1-1 built-in monitoring **on**, running on
the cores SPDK doesn't use). Meant as the reference for the move to Fedora 46 once it is
released, and for later changes.

**Results** (IOPS, except MB/s for the sequential jobs; mean and p99 latency in µs for the QD1
jobs), against the virtio-scsi run of 2026-11-18:

| Job | NVMe/TCP 11-18 | NVMe/TCP 11-19 | iSCSI 11-18 | iSCSI 11-19 |
|---|---|---|---|---|
| `lat-randread` | 1,680 | 1,741 (558 / 1,028) | 1,682 | 1,653 (592 / 1,221) |
| `lat-randwrite` | 3,933 | **2,153** (441 / 2,736) | 3,940 | **2,340** (419 / 3,031) |
| `iops-randread` | 45,628 | 50,568 | 34,319 | 34,133 |
| `iops-randwrite` | 27,884 | **22,240** | 21,938 | **16,521** |
| `mixed-70-30` | 29,039 | 30,476 | 22,745 | 24,968 |
| `seq-read` MB/s | 984 | 1,024 | 788 | 642 |
| `seq-write` MB/s | 513 | 540 | 446 | **371** |
| `dsync-randwrite-qd1` | 3,933 | **1,947** | 4,043 | **1,952** |
| `dsync-randwrite` | 27,285 | **23,155** | 20,803 | **17,730** |
| `dsync-seq-write` MB/s | 549 | 496 | 449 | **392** |
| `fsync-randwrite-qd1` | 3,800 | **1,925** | 3,789 | **2,114** |

**Findings.**
- **Reads are unchanged or better**, so the network path and SPDK's read side didn't regress.
- **Writes are slower, most at queue depth 1 (about half: 3,900 to 2,000 IOPS, 0.25 to 0.44 ms)**
  and by about 20% at depth. Not explained yet (TODO P2-16): between the two runs the build
  changed (P0-4's write-cache handling among others), the built-in monitoring was turned on,
  and the NAS behind the virtual disks may have been busier. The next step is to repeat the
  write jobs with monitoring off, then on the 11-18 build.

## 2026-11-18: the same appliance on virtio-scsi disks

**Setup.** As in the run below, with two changes: the appliance's six pool disks were moved
from emulated SATA to **virtio-scsi** (same virtual disks on the same NAS; they present as
"SYNOLOGY Storage", queue depth 128), and the build is dev-a548637 (one SPDK core). The disks
**refuse to turn their write cache off**, so writes are no longer flushed through to the NAS
as they were on emulated SATA with the cache off: read results compare directly with the run
below, write results don't (part of the write gain is the NAS acknowledging from its cache).

**Results** (IOPS, except MB/s for the sequential jobs), against the tuned emulated-SATA run:

| Job | NVMe/TCP SATA | NVMe/TCP virtio | iSCSI SATA | iSCSI virtio |
|---|---|---|---|---|
| `lat-randread` | 1,652 | 1,680 | 1,590 | 1,682 |
| `lat-randwrite` | 2,334 | 3,933 | 2,283 | 3,940 |
| `iops-randread` | 18,777 | 45,628 | 17,863 | 34,319 |
| `iops-randwrite` | 8,200 | 27,884 | 7,741 | 21,938 |
| `mixed-70-30` | 11,927 | 29,039 | 11,615 | 22,745 |
| `seq-read` MB/s | 643 | 984 | 530 | 788 |
| `seq-write` MB/s | 301 | 513 | 177 | 446 |
| `dsync-randwrite-qd1` | 2,170 | 3,933 | 2,220 | 4,043 |
| `dsync-randwrite` | 8,260 | 27,285 | 7,868 | 20,803 |
| `dsync-seq-write` MB/s | 295 | 549 | 171 | 449 |
| `fsync-randwrite-qd1` | 2,162 | 3,800 | 2,221 | 3,789 |

**Findings.**
- **Deep random reads went from about 18k to 46k IOPS (NVMe/TCP)**, and sequential reads
  reach 984 MB/s, close to 10 GbE. QD1 reads didn't change: they are bound by the round trip
  (NIC, SPDK, disk), not by queueing.
- **The limit moved from the disks to SPDK's single core.** The busiest disk is now 60–94%
  utilised (92–97% before), while the one reactor is 80–95% busy at depth. NVMe/TCP's poll
  group and SPDK's app thread share that core.
- **iSCSI now trails NVMe/TCP by 20–25% at depth** (34k vs 46k random reads). Each iSCSI volume
  is one connection on one poll group, at 90–95% busy; NVMe/TCP has a lighter path per I/O.
- **iSCSI sequential writes caught up** (446 vs 513 MB/s; 177 vs 301 before), so most of that gap
  was the 64K writes queueing on the shallow AHCI queue, not the split itself (P2-14).
- **A second SPDK core didn't help here either** (dev-981f6b3, `-m 0x3`, same disks). Random
  reads over NVMe/TCP rose 5% (48,084), everything else stayed level or fell: random writes
  27,884 → 22,187 (NVMe/TCP) and 21,938 → 19,495 (iSCSI), iSCSI sequential writes 446 → 366 MB/s.
  With two reactors the NVMe/TCP poll groups were only 50–75% busy each, so SPDK was no longer
  the limit, yet writes got slower. Likely cause, not verified: each polling reactor spins a
  vCPU at 100%, and the VM runs on the same 4-core NAS CPU that serves its disks, so a second
  spinning core takes CPU away from the storage behind it (and leaves the guest kernel, which
  does the io_uring and virtio-scsi work, two vCPUs instead of three). An iSCSI volume uses one
  poll group either way. One core stays the default; size it on real hardware (P2-4).

| Job | NVMe/TCP 1 core | NVMe/TCP 2 cores | iSCSI 1 core | iSCSI 2 cores |
|---|---|---|---|---|
| `lat-randread` | 1,680 | 1,740 | 1,682 | 1,691 |
| `lat-randwrite` | 3,933 | 3,513 | 3,940 | 3,851 |
| `iops-randread` | 45,628 | 48,084 | 34,319 | 30,928 |
| `iops-randwrite` | 27,884 | 22,187 | 21,938 | 19,495 |
| `mixed-70-30` | 29,039 | 28,564 | 22,745 | 22,514 |
| `seq-read` MB/s | 984 | 970 | 788 | 785 |
| `seq-write` MB/s | 513 | 504 | 446 | 366 |
| `dsync-randwrite` | 27,285 | 21,936 | 20,803 | 21,457 |
| `dsync-seq-write` MB/s | 549 | 505 | 449 | 370 |

## 2026-11-18: VM appliance on emulated SATA disks, arm64 OpenShift client

**Setup.**
- **Appliance:** a KVM VM with 4 vCPUs (AMD Ryzen V1780B) and 6 GiB RAM. The SAN NIC is
  virtio-net at MTU 9000. The pool is a RAID1 of two 500 GB **emulated SATA** disks (QEMU AHCI,
  "QEMU HARDDISK", disk write caches off), stored on an all-flash NAS with 11 SATA SSDs.
- **Client:** a single-node OpenShift 5.0 cluster, arm64 (160 cores), 10 GbE SAN on the same
  subnet at MTU 9000. Velocor CSI, raw block, StorageClasses for NVMe/TCP and iSCSI on the same
  pool.
- **Builds:**
  - Before: dev-91f9415, the SPDK default of zero-copy sends on, TCP autocorking on.
  - Tuned: the same build with both off at runtime. 7ec49c5 ships that.
  - 2 cores: dev-b33f573 (includes 7ec49c5) with SPDK on 2 cores (`-m 0x3`) instead of 1.

**Results** (IOPS, except MB/s for the sequential jobs):

| Job | NVMe/TCP before | NVMe/TCP tuned | NVMe/TCP 2 cores | iSCSI tuned | iSCSI 2 cores |
|---|---|---|---|---|---|
| `lat-randread` | 20 | 1,652 | 1,581 | 1,590 | 1,666 |
| `lat-randwrite` | 2,072 | 2,334 | 2,327 | 2,283 | 2,372 |
| `iops-randread` | 17,224 | 18,777 | 17,052 | 17,863 | 18,821 |
| `iops-randwrite` | 8,092 | 8,200 | 7,559 | 7,741 | 8,606 |
| `mixed-70-30` | 11,566 | 11,927 | 11,451 | 11,615 | 12,797 |
| `seq-read` MB/s | 604 | 643 | 606 | 530 | 426 |
| `seq-write` MB/s | 295 | 301 | 300 | 177 | 209 |
| `dsync-randwrite-qd1` | 2,076 | 2,170 | 2,295 | 2,220 | 2,379 |
| `dsync-randwrite` | 8,163 | 8,260 | 7,511 | 7,868 | 8,587 |
| `dsync-seq-write` MB/s | 294 | 295 | 290 | 171 | 197 |
| `fsync-randwrite-qd1` | 2,100 | 2,162 | 2,111 | 2,221 | 2,348 |

Latency of the QD1 jobs (tuned, NVMe/TCP): read mean 587 µs, p99 1.04 ms; write mean 405 µs,
p99 0.89 ms.

**Findings.**
- **QD1 reads over NVMe/TCP took about 50 ms each (20 IOPS) until 7ec49c5.** SPDK's NVMe/TCP
  target sends a read's response capsule only after the data send completes. With zero-copy sends,
  a send completes only when the host ACKs it, and Linux ACKs a lone segment late (delayed ACK).
  Turning zero-copy off left about 1 ms, from TCP autocorking holding the small response until
  virtio-net reported the data packet sent. With both off: 1,652 IOPS.
- **The disks are the limit, not SPDK.** At queue depth the busiest pool disk was 92–97%
  utilised, and a second SPDK core changed no job by more than round-to-round noise. With 2
  cores, the NVMe/TCP connections spread over both reactors (each 70–90% busy); an iSCSI volume
  stays on one (about 95% busy), because it has one connection.
- **iSCSI sequential writes are about 40% slower than NVMe/TCP's** (177–209 vs 300 MB/s). SPDK's
  iSCSI target writes each 128K request to the disks as two 64K writes.
- **The durable variants (`dsync`, `fsync`) match the plain writes.** The volumes advertise no
  volatile write cache, so the client sends no flushes. Advertising and honouring a cache is
  L-1 in [TODO.md](../TODO.md).
- **Emulated SATA caps this setup.** The same SSDs reach several times these numbers from a
  client that reaches them without the extra virtual disk layer (deep random reads around 61k
  IOPS, against 17–19k here). QEMU's AHCI emulation has a shallow queue and processes all I/O in
  QEMU's main thread, and every write goes to both mirror halves. Virtual disks for a VM
  appliance should be virtio-scsi with an I/O thread.
