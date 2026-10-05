# Research: Why many metadata backups were kept

Ticket: [#601](https://github.com/djosh34/s3-smb/issues/601). Map: [#572](https://github.com/djosh34/s3-smb/issues/572). Follows [#597](https://github.com/djosh34/s3-smb/issues/597) and [#593](https://github.com/djosh34/s3-smb/issues/593).

JuiceFS details reuse [research-juicefs-deletes.md](https://github.com/djosh34/s3-smb/blob/research/juicefs-deletes/research-juicefs-deletes.md). This report does not redo them.

## Labels

- **Quoted**: the owner's own typed words, copied exactly, typos kept. Source is a session file and a timestamp (UTC).
- **Agent**: text written by an agent. This includes issue bodies and "User confirms" or "Owner answer" comments posted under the djosh34 account. They paraphrase the owner. They are not the owner's words.
- **Docs**: JuiceFS docs at v1.4.1, or s3-smb docs at `v0.2.0`.
- **Source**: read in code or git history.
- **Inferred**: my reasoning.

Session files:

- `pi:planning` is `~/.pi/agent/sessions/--home-joshazimullah.linux-work_mounts-s3-time-machine--/2026-09-27T11-45-01-081Z_01a0e2ae-ca98-72af-81f9-4b87603a687d.jsonl`. This is the owner's 27 September wayfinding chat that set the v0.2.0 scope. It ran in pi with GPT-6 Astra. It is older than the paseo agent list (`paseo ls -a -g` starts on 3 October), so it has no paseo agent ID.
- `cc:6e33f742` is the Claude Code session `~/.claude/projects/-home-joshazimullah-linux-work-mounts-s3-time-machine/6e33f742-4205-4f4f-8ba8-cacb6288a95d.jsonl`. This is the owner's 5 October #572 grilling session.
- `cc:18f42ac6` is the Claude Code session `18f42ac6-582d-4f17-850d-f70ff55ced42.jsonl` in the same folder. This is the owner's 2 October cleanup plan session.
- `handoff` is the file the owner uploaded at the start of `pi:planning`: `~/work_mounts/agent_tools/paseo_host/state/uploads/upload_9956f7a8-0af0-4145-9cc6-508e069b0649/smb-s3-time-machine-handoff.md`. An assistant wrote it in an earlier chat that is not on this machine.

## Short answer

- **The owner never gave a reason for 14 days.** The number came from an assistant in the earlier handoff chat. The handoff itself says it was "assistant recommendations, not separately confirmed user decisions". On 27 September the planning agent proposed 7 days. The owner pushed back with one line: "why no 14 days as default?". The agent agreed at once. No reason was given on either side, beyond the handoff's "Fourteen days gives more recovery time".
- **The owner never asked for many backups.** Their stated need was one recent usable state after the machine dies mid backup, with "e.g. one day ago" as an example. The hourly interval and the thinning up to 2 years are JuiceFS defaults. Agents kept them because the owner said to prefer what JuiceFS already does.
- **The protection guard is agent design.** The owner's own input was "I guess stop and error" when a backup fails. The age check, the "starts closed" rule and the `2 * interval < trash_days` bound were written by agents.
- **The real reason for the trash is in the owner's words, but it only needs a short window.** Old backups only work if their data still exists. The owner wanted "a previously backed up db" to still work after a crash mid backup. That needs the newest good copy to stay whole, not 14 days.
- **JuiceFS keeps backups far longer than their data.** Backups go up to 2 years, data trash defaults to 1 day. The JuiceFS docs only claim backups help within the trash window. Long backup retention protects nothing once the data is gone.

## 1. JuiceFS

All Docs and Source here are JuiceFS v1.4.1, the vendored version.

| What | Value | Label |
| --- | --- | --- |
| Interval | Hourly by default, `--backup-meta` changes it. Skipped above 1 million files at 1 hour | Docs: `docs/en/administration/metadata_dump_load.md`, "Automatic backup" |
| Naming | `meta/dump-YYYY-MM-DD-HHMMSS.json.gz` in the bucket | Source: `pkg/vfs/backup.go`, `rotate` checks the 30 character name |
| Retention | All for 2 days, 1 per day to 2 weeks, 1 per week to 2 months, 1 per month after | Docs: same page, "Backup cleanup policy" |
| End of retention | Deleted after 2 years. The docs do not say this | Source: `pkg/vfs/backup.go:170-178`, comment "delete backups older than 2 years" |
| `trash-days` default | 1 | Source: upstream `cmd/format.go:195-197` |

**What the docs say backups are for.** The backup page says the value of `dump` is "it can export complete metadata information in a uniform JSON format for easy management and preservation", and that it "should be used in conjunction with the backup tool that comes with the database". So it is presented as a portable export, for restore and migration (Docs: `metadata_dump_load.md`). The page has no rule linking backup age to data.

**How retention relates to `trash-days`.** Only the trash page links them: stale slices are kept for the trash period, so "if files are erroneously edited or overwritten, original state can be recovered through metadata backups" (Docs: `docs/en/security/trash.md`). A maintainer said the same on a Time Machine issue: "you can restore JuiceFS using backuped meta from hours ago without data corruption" ([juicedata/juicefs#3030](https://github.com/juicedata/juicefs/issues/3030#issuecomment-1336037247)).

**Does long backup retention protect anything once old data is deleted?** No, for any file that changed. A backup older than `trash-days` points at replaced or deleted slices that may be gone. Files that never changed still read fine. Nothing in JuiceFS links the two settings, and the docs do not warn (Inferred, from the source in research-juicefs-deletes, sections 2 and 4).

So in JuiceFS, the 2 year backup history is mostly a namespace record. Only backups inside the trash window are fully restorable.

## 2. s3-smb v0.2.0

Read from tag `v0.2.0`.

| Part | What it does | Label |
| --- | --- | --- |
| Interval | `backup.interval`, default 1 h, plus a backup at startup | Source: `internal/config/config.go:163`. Docs: `docs/recovery.md` |
| Snapshot | SQLite snapshot, `meta/snapshot-YYYY-MM-DD-HHMMSS.db.gz`, read back and hashed | Source: `internal/backup/backup.go`. Docs: `docs/recovery.md` |
| Retention | Same shape as JuiceFS: all for 2 days, daily to 2 weeks, weekly to 2 months, every 30 days to 2 years | Source: `internal/backup/retention.go:13-16` |
| `trash_days` | Default 14, at most 106751 | Source: `internal/config/config.go:163`. Docs: `docs/configuration.md` |
| Guard | No delete unless the last verified backup started less than `interval + budget` ago. Starts closed, never reopens once expired | Source: `internal/backup/protection.go:14-74` |
| Bound | `interval + budget < trash_days * 24h`. The budget is one interval, so `2 * interval < trash_days * 24h` | Source: `protection.go:38-40`. Docs: `docs/configuration.md`, "Retention" |
| Recovery | Asks before restoring the newest backup, checks it, wipes the volume cache, takes a new backup before serving. Never falls back to an older backup by itself | Docs: `docs/recovery.md`, "Recover on a new machine" |

**Why, as written in the code and docs.**

- `protection.go`: "Protection allows deleting data only while a recent metadata backup exists." The interval plus budget "must stay below the JuiceFS trash retention" (Source).
- `docs/recovery.md`, "Why retention matters": "An old metadata backup may point at data blocks that no longer exist." A backup from before a compaction "can no longer restore those files" once the old blocks leave the trash (Docs).
- `docs/recovery.md`, "Terms": a recovery point "is usable only while the data blocks it points at still exist" (Docs).
- The docs never claim a 14 day restore window. The planning issue said so outright: "Fourteen days is the native trash setting, not a promise that every saved metadata file remains recoverable for exactly fourteen days" (Agent: [#9 body](https://github.com/djosh34/s3-smb/issues/9)).

Note the mismatch: the bound only needs the trash to outlive about 2 hours. 14 days is 168 times that. Nothing in the code needs 14 days (Inferred).

**Git history** (Source, `git log -S` and commit messages):

| Commit | Date | What |
| --- | --- | --- |
| `5363c88` | 27 Sep | "require encrypted metadata backups and fresh-install recovery" |
| `4a4532e` | 27 Sep | "accept recovery since last successful metadata backup" |
| `2dc4e42` | 27 Sep | "set hourly metadata backup and 14-day trash defaults". README: "retain deleted/replaced data through JuiceFS trash for 14 days by default". No reason in the message |
| `5cddf40` | 27 Sep | "stop on failed scheduled metadata backups" |
| `ba1f7f8` | 27 Sep | Implementation contract: "Use native metadata-export retention. This does not promise that every old export still has all its data blocks." |
| `0856e0b` | 29 Sep | First code: `TrashDays: 14`, `ErrUnprotected` |
| `e07369e` | 2 Oct | Budget raised to one interval: "Delete protection now lasts two intervals after a backup, so startup requires 2 * backup.interval < backup.trash_days * 24h" |
| `04899f8` | 4 Oct | SQLite snapshots replace JSON dumps, `expiredSnapshots` copies the JuiceFS thinning |

All these commits are agent work under the owner's git identity.

## 3. The owner's own words

### 27 September, planning (`pi:planning`)

The thread on backups, in order. Owner lines are **Quoted**. Agent lines are marked.

1. 12:15:35, owner, Quoted. This is where metadata backups were first asked for:

   > Also dont forget that the embeddeddb, or metadatadb or whatever it is called, the thing from juicefs, must do checkpoints to s3 in order to make sure that a pure s3 only access from fresh installation of this thing without ever having anything else saved from the broken server/macbook it ran on, still everything can be recovered.

2. 12:19:37, agent asked how much recent work recovery may lose, and suggested "a configurable interval, initially hourly".

3. 12:23:13, owner, Quoted. This is the only stated need behind keeping older backups:

   > Q1 since last succesfull backup. everything else more direct 'synced' will probably slow this whole ordeal so much down it will be unusable. basically, even when the mac was backing up, and then blows up, or falls in the water, or whatever, you can still recover some previous and recent backup. e.g. one day ago
   >
   > I thought that juicefs already provided the option to do that right? with reasonable rpo level for the recovery.
   > Dont overengineer, keep it stupid simple. Less overengineering means less can go wrong. be pragmatic

   The agent recorded this as: "Yesterday is an example of an acceptable recovery state, not a newly agreed fixed retention period or freshness guarantee" (Agent: [#6 body](https://github.com/djosh34/s3-smb/issues/6)).

4. 12:33:30, agent proposed the defaults:

   > ❓ **Q2 — Backup defaults:** Start with hourly metadata backups and seven-day JuiceFS trash retention, both configurable?
   >
   > ➡️ **Yes.** Trash retains replaced/deleted blocks so recent metadata backups still have their data. It does not create seven full copies. We'll report failed backups and prevent cleanup from destroying the last usable recovery copy.

5. 16:10:32, owner, Quoted. This is the whole origin of 14 days:

   > Q2 yes, but why no 14 days as default?

6. 16:10:57, agent:

   > Fourteen days is fine. Seven was an unnecessary change from the earlier proposal. I'll record hourly metadata backups and 14-day trash retention.

   "The earlier proposal" is the handoff.

7. 16:41:26, owner, Quoted. This is the owner's only input on what happens when backups fail:

   > Q2 uhh bit more details. How can metadata backup fail but s3 work then. I guess stop and error but ur question doesn't make sense. Like how would this happen but like the full smb2 mount is fine and only backup fails?

   The question the owner answered was the agent's 16:39 proposal: "stop before cleanup can destroy the last usable recovery copy. Apply the same check on startup after a long shutdown." The owner accepted stop and error. The agent then wrote "validate safety before cleanup after downtime" into the issue (Agent: [#14 comment](https://github.com/djosh34/s3-smb/issues/14#issuecomment-5857802561)). The owner did not ask for the age check or the guard by name.

The owner never mentioned backup retention counts, the 2 year thinning, or the guard in this session. I searched every owner message in it (29 messages).

### Where 14 days came from (`handoff`)

The handoff was uploaded by the owner but written by an assistant. Agent text throughout:

> The proposed safety policy is 14-day JuiceFS trash retention, hourly metadata exports, and an additional consistent SQLite backup after each completed Time Machine run. Those numbers and the completion routine were assistant recommendations, not separately confirmed user decisions.

> Seven days was considered workable if failures are noticed quickly. Fourteen days gives more recovery time. Measure actual retained-object churn before deciding whether the cost justifies shortening it.

Its table also said: "Retain completed recovery points: At least the last 7 days, within the 14-day block-retention window". And it paraphrased the owner: "Reliability | Primary user requirement | A crash during today's backup must not make yesterday's protected recovery point unusable." That row is the assistant's summary of the owner, not a quote. I could not find the original chat.

So the chain is: earlier assistant picks 14 days for "more recovery time" → planning agent proposes 7 → owner asks "why no 14" → agent agrees. The churn measurement the handoff asked for was never done before the default shipped (Inferred, from git history and issues).

### 2 October, cleanup plan (`cc:18f42ac6`)

16:05:23, owner, Quoted. This is the clearest statement of what backups must protect:

> The main thing that needs rigorous testing is the fact that juicefs requires a local database. Well if ur mac fails during a partial backup, thus with a partial metadata written or whatever can happen, we must be certain that even in that state, a previously backed up db works with the then the current s3 state that already has blocks of this extra backup. Shouldnt be one test but preferably multiple scenarios.

This became [#46](https://github.com/djosh34/s3-smb/issues/46), "Prove older backups survive a failed backup". It asks for "a previously backed up db", not a long history.

### 5 October, #572 grilling (`cc:6e33f742`)

The owner's words on copy retention, all Quoted:

- 16:17:30: "Sqlite full backup copied every 15 min. ... Only when db backup is trashed after n days, to be deleted chunks are truly deleted."
- 16:48:40: "Q16 what about custom thing like tm. So last day keep every 15 min. Last 3 days keep every hour. Last week keep every 4 hour"
- 17:01:04: "Q16 A flat 7 days feels simpler and safer". The 7 days option was the agent's recommendation on [#593](https://github.com/djosh34/s3-smb/issues/593): "B. It is a week to notice a broken copy, with half the waste of 14 days." Recorded as "Owner answer, round 8" (Agent).
- 19:29:11: "Why not wait like 2 db backups and already then delete in the blunt wait instead of 700. ... It never asked from us you need all 15 min intervals of last 7 days. It needs instead one valid state, even mid backup, cuz it is made to restore from partial backups (it has to be able to do that). So why is blunt wait, waiting for 700 copies?"
- 19:38:58: "What was actually the original reason for keeping so many backups of the db?"
- 19:40:43: "How does juicefs do it? Could you also query me saying that in paseo and why i wanted it?"

The last two started this ticket. The owner chose the last 4 copies on [#597](https://github.com/djosh34/s3-smb/issues/597).

### Searched, nothing found

- GitHub: issues #1 to #34 (the first map), plan #147, parked #173, #46, #60, #572, #590, #592, #593, #597, with comments. Every owner decision there is agent text, written as "User confirms", "User decisions" or "Owner answer". None gives a reason for 14 days or for long backup retention.
- Paseo: `paseo ls -a -g` (about 200 agents, from 3 October). Agent prompts there come from coordinators, not the owner. No owner message on retention.
- Pi sessions in `~/.pi/agent/sessions/...s3-time-machine--/`: the owner-driven ones are `pi:planning` and the 29 and 30 September status chats. Only `pi:planning` touches retention.
- Claude Code transcripts: owner-driven sessions are `18f42ac6`, `232e2b16`, `3c8ee94c` (3 October plan #147) and `32145d8d`, `6e33f742` (#572). Only `18f42ac6` and `6e33f742` touch backups.
- `~/.paseo`: daemon logs and config only.

## 4. Answer

| Question | Answer | Label |
| --- | --- | --- |
| Who asked for metadata backups? | The owner, for fresh-install recovery from S3 only | Quoted, 27 Sep 12:15 |
| Why keep older backups at all? | So a crash during a backup still leaves "some previous and recent backup. e.g. one day ago" | Quoted, 27 Sep 12:23 |
| Why hourly? | JuiceFS default, proposed by the agent | Agent, Docs |
| Why thinning up to 2 years? | JuiceFS default, kept because the owner said to prefer what JuiceFS does. Never discussed | Agent (contract `ba1f7f8`), Source |
| Why 14 days of trash? | An earlier assistant chose it for "more recovery time". The owner asked for it back over 7 days without a reason | Agent (handoff), Quoted 27 Sep 16:10 |
| Why the guard? | The owner said stop and error on a failed backup. Agents turned that into an age check on every delete | Quoted 27 Sep 16:41, Agent, Source |
| Does a long trash protect older backups? | Yes, but no backup older than the newest is ever restored automatically, and the owner's need was one recent good state | Docs, Inferred |
| Does long backup retention help once data is gone? | No, in JuiceFS or v0.2.0, for files that changed | Inferred |

**Bottom line (Inferred).** No reason was ever given for keeping many backups or a 14 day trash. Both came from defaults: JuiceFS's thinning, and an assistant's "more recovery time". The owner's real need, in their own words, was one recent valid state after a crash mid backup. That matches the 4 copy choice on #597.
