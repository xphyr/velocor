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
