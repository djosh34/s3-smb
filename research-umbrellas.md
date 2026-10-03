# Triage of umbrella issues #67-#83 (s3-smb)

Read-only research, 2026-10-03. Sources: every issue #58-#146, including bodies and all comments (fetched with `gh issue view --json`), plus the GraphQL sub-issue tree. Nothing was modified.

## Main finding

**None of the ten security reviews (#68-#77) produced any output.** Each has only its one-line body ("The reviewer posts its full review here as a comment") and no comments. #67 says an earlier GPT-6 Astra run on these issues hit an OpenAI cybersecurity filter and was archived. The GPT-6.1 Sol rerun also posted nothing. #83 states this directly: "The ten security reviews (#68 to #77) could not run, so this list does not cover security." So there are no security findings to deduplicate, and v0.1.0 has had no security review.

## 1. What each umbrella holds, and how it relates to other issues

- **#67 "Independent code reviews of v0.1.0"**: a parent issue with no content of its own. Its GitHub sub-issues are #68-#82. It explains who reviewed what and what context they were given (#43, #1).
- **#68-#77, security reviews 1-10**: empty (see above).
- **#78-#82, code reviews 1-5 (security excluded, GPT-6 Astra)**: each holds one long review comment covering scope, validation, findings, test gaps, maintainability and a suggested fix order. Findings per review: #78 16 (F1-F16), #79 17, #80 14 + G1-G3, #81 14 + S1/S2, #82 16. The reviews overlap heavily, and the same ~10 core defects appear in 4-5 reviews each.
- **#83 "Findings from the v0.1.0 code reviews"**: the deduplicated outcome. Its GitHub sub-issues are exactly #84-#134 and #144-#146 (54 issues). #84-#130 come from the five reviews and the #64 investigation. Each sub-issue records which reviews reported it, with comment links, finding IDs and each reviewer's severity. #131-#134 and #144-#146 were added later from "Separate defect:" comments on #64. #59 got a comment instead of a new issue, covering reviews 3 and 5 on the global lock held across I/O.
- **Not under #83:** #135-#143. These are owner-driven follow-ups that use the review findings. #140 (robustness) is the parent of #135, #136, #137, #141 and #142. #139 (SMB library problems) is the parent of #138. #143 has no parent. #135 extends #95, #138 extends #122, and #136/#137 generalise #87.

**Yes: #84-#146 are the deduplicated result of the five code reviews and #64.** I mapped every counted finding in every review to an issue:

| Review | Mapping |
|---|---|
| #78 R1 | F1 #84, F2 #89, F3 #90, F4 #85, F5 #86, F6 #87, F7 #88, F8 #102, F9 #96, F10 #94, F11 #103, F12 #104, F13 #99, F14 #105, F15 #118, F16 #95. Gaps: #120, #116, #122, #101, #121. Docs: #127 |
| #79 R2 | 1 #91, 2 #84, 3 #89, 4 #85, 5 #86, 6 #87, 7 #88, 8 #98, 9 #96, 10 #100, 11 #107, 12 #94, 13 #97, 14 #108, 15 #109, 16 #110, 17 #95. Gaps: #116, #120, #125, #121 |
| #80 R3 | R1 #84/#89, R2 #90, R3 #85, R4 #86, R5 #87, R6 #98, R7 #88, R8 #94, R9 #97, R10 #101, R11 #111, R12 #106, R13 #99, R14 #95, G1 #116, G2 #117, G3 #119. Lock note: #59. Memory: #121. Structure: #124 |
| #81 R4 | 1 #92, 2 #91, 3 #84, 4 #89, 5 #90, 6 #87, 7 #85, 8 #86, 9 #88, 10 #96, 11 #94, 12 #98, 13 #97, 14 #93. S1 #123, S2 #124. Coverage: #92, #116, #95/#122, #121, #120 |
| #82 R5 | 1 #91, 2 #84, 3 #90+#93, 4 #85, 5 #86, 6 #88, 7 #87, 8 #100, 9 #94, 10 #97, 11 #96, 12 #113, 13 #112, 14 #95, 15 #114, 16 #115. Gaps: #116, #120, #126, #121, #127. Lock: #59 |

All 13 "Separate defect:" comments on #64 are also filed: #128, #84, #89, #129, #130, #131, #132, #133, #104 (as a comment), #134, #144, #145, #146.

I also checked secondary points buried inside findings, and each one is in the matching issue body:
- #82's filesystem-ID point is in #87.
- #82's `FILE_OPEN_IF` create action is in #94.
- #82's CLOSE post-query attributes are in #97.
- The lease-context echo (#80, #82) is in #85 and #138.
- The ignored `StatFS(0)` error (#78, #81) is in #136.
- The writable-restart stale-lock window and the `docs/development.md` wording are in #101.
- The `fileTree.setRename` vs `smbfs.Rename` path split (#80) is in #124.
- The writer-preferring `RWMutex` point (#82) is in #59.
- The "pending retirement after import" test (#81) is in #92.
- The positive-cache recovery test is in #91 and #116.

## 2. Findings in #67-#83 not tracked by #58-#146

Nothing of substance is lost. All 77 counted findings and every test, maintainability or doc note map to an issue. These leftovers are minor or were never counted as findings:

1. **The security review itself (high, process).** It never happened. Closing #68-#77 loses no findings, but it does drop the only record that a security review was planned. That needs a new issue or a decision (see section 3).
2. **E2E fixture fragility (low, `test/e2e` helpers and `test/run-linux.sh`).** #82, under "for completeness": on a host-run integration attempt, the transport tests failed because the `transport.test` host mapping was missing outside Docker. Also, one cold-recovery test went past the helper's 20-second client context while two suites ran at once. The reviewer said this was not a product finding. It is untracked, and could go under #142 or #126 if wanted.
3. **Environments never qualified (informational, docs).** #80 and #82 note there is no evidence for Apple Silicon, Windows or Linux kernel SMB clients, real AWS S3 or other providers, power loss, or large installations. Both say the README already limits its claims correctly and ask only that the docs stay that narrow. #127 partly covers this (it keeps the "tested environment" claims narrow). Low value as a separate issue.
4. **Excluded experiment (none).** #79 tried truncate-vs-byte-range-lock and dropped it as not a defect under MS-FSA. Nothing to track.
5. **Things the reviews did not cover (informational).** They did not certify untouched vendored code (~93,500 Go lines), and the vendored trees got no cryptographic audit. That overlaps with item 1.

## 3. Security findings

**There are no security findings: all ten security reviews are empty.** No deduplication is possible, and no covering issues exist for them.

For context, here are the security-relevant items that already exist as correctness or robustness issues. Severity is for a LAN-only Time Machine server with one password-protected account. The default listener is `127.0.0.1:445`, NTLM authentication is required, and requests without a session are rejected (docs/vendored.md):

| Item | Issue | LAN risk |
|---|---|---|
| An authenticated client can crash the daemon with an ordinary query (`FileFsObjectIdInformation`, FSCTL), stopping any running backup | #87, #137 | Medium. Denial of service by an authenticated user. The real risk is accidental crashes by macOS itself, not an attacker. |
| Parse results and errors ignored in about 20 handlers (`res, _ := accept(...)`) and in NEGOTIATE and SPNEGO paths (`makeResponse`, `initSecContext`, `EncodeNegTokenRejected`), some of which run before authentication | #136 | Low-medium. Malformed input could reach nil or garbage state. The pre-auth paths are the only part reachable without a password. |
| Explicit panics in the vendored SMB crypto (`ccm.go` nonce or ciphertext length, `cmac.go`) and in `vfs/attributes.go` | #137 | Low-medium. These are reachable with malformed signed or encrypted traffic, probably only after a session exists. |
| zstd decompression writes past the requested range of a read buffer | #131 | Data integrity rather than memory safety (Go bounds checks). Low as a security issue. |
| Credit accounting errors (double grant, zero grant, drained credits) | #132-#134 | Low. Interop and denial of service only. |
| Misframed sender stream and cross-talk between concurrent responses | #129, #130 | Low as security, though one response could reach the wrong request on the same connection. Same user only. |
| Share modes and oplocks ignored | #85, #86 | The reviewers said explicitly this is not access control. Not a security risk with one account. |
| Local SQLite metadata and cache are not encrypted at rest (an explicit startup confirmation says so) | none (design) | Accepted by design, decided in #17 and #43. |

Areas a real security review would still need to cover, and which nobody has examined: NTLM/SPNEGO implementation and replay, whether signing is enforced, behaviour when `smb.listen` is widened to `0.0.0.0` on a LAN, path traversal through SMB names and stream names (`file:stream`, `..`), secret handling in config and logs (#26 covered slog redaction at the time), the S3 credential source subprocess (`config/secret.go` kills a process group), and malformed-packet fuzzing of the vendored server. For LAN-only use with a loopback default, the realistic threats are denial of service and robustness, and #137, #136 and #142 already aim at those.

## 4. Which umbrellas can be closed as superseded

- **#78, #79, #80, #81, #82: close.** Every finding is tracked in #84-#127 or #59, with back-links to the exact review comment. The review comments stay readable after closing, and the sub-issues link to them.
- **#83: can stay open as the tracking parent until #84-#134 and #144-#146 are done, or be closed now.** Its sub-issues stay linked either way. It is a checklist, not a review, so keeping it open does no harm. Keep the issue alive while the sub-issues rely on its list for severity and counts.
- **#68-#77: close as "not completed".** Before or at the same time, open one new issue such as "Security review of s3-smb (the v0.1.0 attempts produced nothing)". Otherwise the gap disappears silently. Close them as not planned or not completed, not as superseded, because nothing replaced them.
- **#67: close once #68-#82 are closed.** It has no content beyond the list of sub-issues.
