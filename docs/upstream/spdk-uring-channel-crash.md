# bdev_uring: spdk_tgt crashes when io_uring can't be set up

Draft upstream report for SPDK (found with v26.09, `0bbb7fe`).

## Summary

When `io_uring_queue_init()` fails in `bdev_uring_group_create_cb()` (for example because
io_uring is disabled, blocked by SELinux or seccomp, or the ring can't be allocated),
`spdk_tgt` crashes with a segmentation fault instead of failing the bdev's I/O channel. Every
other bdev and target in the process goes down with it.

## Reproduction

Run `spdk_tgt` where `io_uring_setup()` returns `EPERM`. One easy way is a rootless Podman
container on Fedora with SELinux enforcing, which runs the process as `container_t`:

```
truncate -s 64M /tmp/u/disk.img
podman run -d --name t --network host -v /tmp/u:/tmp/u:z --entrypoint spdk_tgt <spdk image> \
  -r /tmp/u/spdk.sock -m 0x1 -s 1024 --no-huge --no-pci
rpc.py -s /tmp/u/spdk.sock bdev_uring_create /tmp/u/disk.img u0 512
```

The RPC doesn't return; the process exits with status 139:

```
bdev_uring.c: 837:bdev_uring_group_create_cb: *ERROR*: uring I/O context setup failure
thread.c:2449:spdk_get_io_channel: *ERROR*: could not create io_channel for io_device uring_module (0x79a140): Operation not permitted (rc=-1)
```

The same container with `--security-opt label=disable` (type `spc_t`) creates the bdev
normally.

## Cause

`bdev_uring_create_cb()`, the per-bdev channel constructor, gets the module's group channel
and uses it without checking for failure:

```c
static int
bdev_uring_create_cb(void *io_device, void *ctx_buf)
{
	struct bdev_uring_io_channel *ch = ctx_buf;

	ch->group_ch = spdk_io_channel_get_ctx(spdk_get_io_channel(&uring_if));

	return 0;
}
```

When the group channel's constructor fails, `spdk_get_io_channel()` returns `NULL`, and
`spdk_io_channel_get_ctx(NULL)` yields a pointer just past address 0. The channel is then
reported as created, and the first use of `group_ch` (examine I/O right after
`bdev_uring_create`) dereferences it.

## Proposed fix

Fail the channel instead:

```c
static int
bdev_uring_create_cb(void *io_device, void *ctx_buf)
{
	struct bdev_uring_io_channel *ch = ctx_buf;
	struct spdk_io_channel *group_ch = spdk_get_io_channel(&uring_if);

	if (group_ch == NULL) {
		SPDK_ERRLOG("Could not get the uring group channel\n");
		return -ENOMEM;
	}
	ch->group_ch = spdk_io_channel_get_ctx(group_ch);

	return 0;
}
```

The bdev layer then fails the channel request, and `bdev_uring_create` returns an error to the
caller.

## Workaround in Velocor

Until this is fixed, velod checks whether `spdk_tgt` can use io_uring before every SSD attach,
and uses AIO when it can't (`--ssd-bdev auto`, the default; `internal/hardware/iouring.go`).
