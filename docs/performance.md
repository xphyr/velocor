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
  P0-4 in [TODO.md](../TODO.md).
- **Emulated SATA caps this setup.** The same SSDs reach several times these numbers from a
  client that reaches them without the extra virtual disk layer (deep random reads around 61k
  IOPS, against 17–19k here). QEMU's AHCI emulation has a shallow queue and processes all I/O in
  QEMU's main thread, and every write goes to both mirror halves. Virtual disks for a VM
  appliance should be virtio-scsi with an I/O thread.
