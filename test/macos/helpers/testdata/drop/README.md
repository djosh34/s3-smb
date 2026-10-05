# Drop log samples

`ParseDropLog` tests use these samples. Each file holds one message from
SMBClient-494.120.2, `kernel/netsmb/smb_iod.c`, in the format of
`log show --style json`:

- `refused.json`: `smb_iod_reconnect`, line 3352. macOS refuses to reconnect
  while non-idempotent requests, such as CREATE, LOCK and SET_INFO, are in
  flight.
- `reconnected.json`: `smb_iod_reconnect`, line 3754. Recorded on macOS 15.7
  in Mac run 37305839289, with fields the parser does not read left out.
- `failed.json`: `smb_iod_reconnect`, line 3563, with the test share and server.

Source: https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-494.120.2/kernel/netsmb/smb_iod.c

The refusal is logged with `SMBWARNING`, which a stock Mac hides at
`net.smb.fs.loglevel=0`. The network scenarios set level 1 after the baseline
mount loads smbfs and restore the previous level during cleanup. The success
message uses `SMBERROR` and needs no setting.

`refused.json` and `failed.json` are written from Apple's source, not recorded:
their JSON envelope, function prefix and channel ID are illustrative. Replace
them with real `log show` output once a Mac run records them.
