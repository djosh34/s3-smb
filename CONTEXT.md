# SMB-to-S3 storage

The daemon exposes remote file data through SMB. It preserves the upstream SMB server's Time Machine features. Tests with a real Time Machine client come later.

## Language

**Remote dataset**:
The files and directories stored through the daemon in object storage. It may exceed the daemon's available local storage.

**Local data cache**:
The locally retained subset of remote file data used to avoid repeated downloads. It is not a complete local replica of the remote dataset.
