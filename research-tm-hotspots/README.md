Analysis scripts for `research-tm-hotspots.md` (#598). Input: the
application log and harness log from the `mac-hotspots-*` artifact.

```sh
python3 hotspots.py s3-smb-mac-evidence/application-1-initialize.log mac-harness.log out.json
python3 report.py out.json
```

`hotspots.py` replays the SMB trace into a chunk table at 8 MiB, 1 MiB,
256 KiB and 4 KiB, and writes versions, trash, JuiceFS bytes, hot spots and
rows per backup as JSON. The backup windows come from the
`hotspots-backup-start` and `hotspots-backup-end` lines in the harness log.
For older traces, pass `label=START,END` pairs instead. `report.py` prints the
Markdown tables used in the report.
