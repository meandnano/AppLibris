# Close the second review round: the "whichever door" claim, and two nits

## Context

The re-review of `1de508c` confirmed all six earlier findings closed and
filed three new ones. The first is a claim that commit wrote into three
places and that was not true when written; the other two are consequences of
the consolidation that went in with it.

## 1. A description's blank lines really are capped by its parser

`1de508c` justified `capValue` not capping a description's blank lines on
the grounds that "a description's blank lines are capped by whichever parser
produced it", and wrote that into `internal/scanner/scanner.go`, `CLAUDE.md`
and `docs/notes/scanner.md`. Two of the doors those sentences name did not
do it.

`internal/fb2`'s `annotationText` trims each `<p>` and joins with `"\n\n"`,
but a `<p>` is chardata: a paragraph wrapped across source lines keeps every
interior break, and `capValue`'s description branch only trims the edges. An
FB2 book therefore rendered exactly the multi-line gap the first round filed
for EPUB. `annotationText` now ends on `storage.CapBlankLines`, the call
`PlainDescription` also ends on, which makes the sentence true rather than
narrowing it — `internal/fb2` already imports `internal/storage`.

`internal/service`'s `normalizeField` does not cap either, and should not:
the blank lines a person typed are their own. The CLAUDE.md bullet said
otherwise by sitting under "All three writers"; it now names the three
callers that do cap and says plainly that a person's edit is not one.

## 2. `maxReferenceName` was 32 for a 31-byte reason

`CounterClockwiseContourIntegral` is 31 bytes; 32 is its length with the
semicolon, which `referenceAt`'s span does not cover. The constant is 31,
and the comment says what the figure is and that it bounds a numeric
reference's digits by the same count — where HTML has no limit, so `&#` and
thirty-two leading zeros is a legal spelling this leaves as text. That is
the safe direction for a bound whose only job is to stop an unterminated `&`
scanning to the end of a description.

## 3. `CapBlankLines` is one pass

Moving `enrich.capBlankLines` down put its fold-trim-collapse spelling on a
path that runs before anything is capped: `internal/epub` bounds a package
document at 4 MiB and `internal/scanner` cuts a description to 64 KiB only
afterwards. On four megabytes of newlines that spelling costs 40
allocations and 81 MB; one pass costs 1 and 5.6 MB, and runs in 6 ms against
148 ms.

The rewrite keeps the exported name and behaviour and is verified rather
than reasoned about: `TestCapBlankLinesMatchesTheObviousSpelling` keeps the
fold-trim-collapse version as an oracle and runs 400,000 random inputs over
an alphabet of newlines, carriage returns, CRLF pairs, spaces, tabs and
text through both.

## Verification

`go test ./...`, `go vet ./...`, and `go test -race` over `internal/storage`,
`internal/fb2`, `internal/epub`, `internal/scanner`, `internal/enrich`,
`internal/googlebooks` and `internal/web`.

Both new guards are mutation-checked: dropping `storage.CapBlankLines` from
`annotationText` fails the FB2 test with the uncapped run in the message,
and the differential test fails on any disagreement with the oracle.
