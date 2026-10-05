Analysis scripts for `research-tm-mac-trace.md` (#528). Each takes the
application log from the `mac-backup-*` artifact:

```sh
python3 analyze.py  s3-smb-mac-evidence/application-1-initialize.log  # counts, streams, renames, flushes, handles
python3 phases.py   s3-smb-mac-evidence/application-1-initialize.log  # per-phase counts (times are for run 37289256176)
python3 timeline.py s3-smb-mac-evidence/application-1-initialize.log  # per-band event timeline
python3 final.py    s3-smb-mac-evidence/application-1-initialize.log  # 8 MiB positions per band, write scatter
```
