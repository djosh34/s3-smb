# Ported source

s3-smb ports two small pieces of github.com/macos-fuse-t/go-smb2 at commit
`277a9300411249a881a05f7a910f5a83ae3395f2` into its own packages. Each ported
file carries the line `// Modified for s3-smb, 2026. See docs/vendored.md.`

## SMB authentication

`internal/smb/auth/ntlm.go` and `crypto.go` port the reviewed NTLMv2 constants
and authentication formulas from github.com/macos-fuse-t/go-smb2 at commit
`277a9300411249a881a05f7a910f5a83ae3395f2`. They keep the upstream AGPL licence
and attribution in `internal/smb/auth/LICENSE` and
`internal/smb/auth/Attributions.txt`. The decoders, SPNEGO wrapper and exchange
state are written here, not copied.

The review found wrapping uint32 offset sums, unchecked short response slices,
unchecked MIC layouts, input mutation while checking the MIC and nonconstant
proof comparisons upstream. The port uses widened bounds checks,
validated AV pairs, verification of each supplied MIC, private transcript copies
and constant-time proof comparisons. SPNEGO selects NTLM anywhere in the client
list and requires a mechanism-list MIC when NTLM is not the first choice, as
RFC 4178 requires. It removes guest, anonymous, NTLMv1 and NTLM
session sealing. MD4, HMAC-MD5 and RC4 remain only where MS-NLMP requires them.
`auth_test.go` checks the MS-NLMP section 4.2.4 proof and key vectors and the test
initiator exchange. Decoder tests and fuzz targets cover malformed tokens.

## SMB crypt package

`internal/smb/crypt/cmac.go` ports the AES-CMAC algorithm from
`internal/crypto/cmac/cmac.go` of go-smb2 at the same commit. Review checked
its subkey doubling, final-block handling and RFC 4493 padding. The port is
AES-only, has call-local state, removes the panic and streaming hash interface,
and uses fixed-size arrays.
`internal/smb/crypt/cmac_test.go` checks all four RFC 4493 vectors.
The file retains the Go Authors and Hiroshi Ioka copyright and BSD-3-Clause
notice.

## Rules

`go install module@version` ignores `replace` directives, so the module must
build from its own source alone.

- No `replace` directive in `go.mod`.
- No `go.mod`, `go.work` or `vendor` under `internal/`. A nested `go.mod` drops that directory from the published module.
- No import of the original go-smb2 path.

`packaging_test.go` checks all three. `scripts/check-public-install.sh vX.Y.Z` installs a published version from empty caches.

## Licences

s3-smb's own code is AGPL-3.0-only. Each ported file keeps its upstream licence and copyright notice: AGPL-3.0 for the go-smb2 port in `internal/smb/auth`, BSD-3-Clause for the AES-CMAC port. Keep `NOTICE`, `internal/smb/auth/LICENSE` and `internal/smb/auth/Attributions.txt` in any copy. If you distribute a modified version, publish the source of that version.
