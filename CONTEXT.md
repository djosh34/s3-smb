# SMB-to-S3 storage

The daemon exposes remote file data through SMB. It preserves the upstream SMB server's Time Machine features. Tests with a real Time Machine client come later.

## Language

**Remote dataset**:
The files and directories stored through the daemon in object storage. It may exceed the daemon's available local storage.

**Local data cache**:
The locally retained subset of remote file data used to avoid repeated downloads. It is not a complete local replica of the remote dataset.

**Metadata backup**:
A saved copy of the filesystem's directory entries, file attributes, and data mappings needed to recover access to its files. It is not a SQLite WAL checkpoint or a Time Machine backup.

**Recovery point**:
The filesystem state recorded by a successful metadata backup. It is usable only while its referenced data remains available.

**Metadata authority**:
The live metadata that determines the remote dataset's namespace and data mappings. A dataset has only one writable metadata authority at a time.

**Fresh-install recovery**:
Restoring the daemon's filesystem using S3 and secrets kept outside the old machine, without its local database, cache, or configuration files.
