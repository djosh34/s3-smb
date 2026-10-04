# Drop log samples

These are source-derived samples, not Mac recordings. Each JSON file uses an
exact message from Apple's SMBClient-494.120.2, `kernel/netsmb/smb_iod.c`:

- `refused.json`: `smb_iod_reconnect`, line 3352. The preceding call checks
  in-flight non-idempotent requests, including CREATE, LOCK and SET_INFO.
- `reconnected.json`: `smb_iod_reconnect`, line 3754.
- `failed.json`: `smb_iod_reconnect`, line 3563, with test share and server values.

Source: https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-494.120.2/kernel/netsmb/smb_iod.c

The JSON envelope, function prefix and channel ID are illustrative. The
integration agent will replace these samples with bounded log-show recordings
from the first Mac runs. No fixture is evidence of a successful Mac test.
