# SMB-to-S3 storage

The daemon exposes remote file data through SMB. Time Machine is a later client integration, not the definition of the storage service.

## Language

**Remote dataset**:
The files and directories stored through the daemon in object storage. It may exceed the daemon's available local storage.

**Local data cache**:
The locally retained subset of remote file data used to avoid repeated downloads. It is not a complete local replica of the remote dataset.

**Protected recovery point**:
A saved filesystem state recoverable after loss of all local daemon state, using remote metadata, its referenced data objects, and recovery secrets. It does not by itself mean a completed Time Machine backup.
_Avoid_: SQLite WAL checkpoint, completed Time Machine backup
