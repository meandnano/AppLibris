# Close review findings 1–6 on `PlainDescription` and `capValue`

## Context

The review panel over `ui-tweaks` (PR #79) returned six actionable findings,
all minor, none blocking. They cluster around one root cause: moving
`plainText` from `internal/googlebooks` into `internal/storage` as
`PlainDescription` gave it a second caller whose input looks nothing like
the first's. Google always sends HTML; an EPUB's `dc:description` is often
ordinary prose. Three of the six are the function behaving differently on
those two inputs in ways nobody chose.

The previous plan is in `docs/plans/completed/` and is immutable, so this is
a new plan: `docs/plans/2026091102-review-findings-plain-description.md`.

The findings, in the order this plan fixes them:

1. `PlainDescription`'s fast path (`!strings.ContainsAny(raw, "<&")`) skips
   the blank-line collapse and returns only a trim, so whether blank lines
   are capped depends on whether the text happens to contain an ampersand.
   Verified: `"a\n\n\n\n\nb"` is returned whole, `"a\n\n\n\n\nb & c"` is
   capped at two. Under `white-space: pre-line` that is a visible five-line
   gap for an EPUB description, where the same text from a provider is
   capped.
2. `capValue`'s new comment, and the CLAUDE.md and `scanner.md` invariants
   widened alongside it, claim every stored value is one the editor accepts.
   A description under the limit is returned untouched — no trim — so the
   claim holds only because both parsers happen to trim.
3. `html.UnescapeString` decodes HTML5's semicolon-less legacy references.
   Verified: `"Rock &copy roll"` → `"Rock © roll"`. Correct for Google, whose
   input is HTML; wrong for EPUB prose that was never markup, and with no
   backfill there is no way back.
4. `internal/googlebooks/googlebooks_test.go:1009` still names `plainText`,
   which no longer exists in that package.
5. The `added` alignment fix has no regression assertion, and deleting
   `.editable--meta dd` left `flex: 1; min-width: 0` as the only thing
   holding every value flush-left — which reads as redundant.
6. A double-escaped `&amp;#13;` in an OPF reaches `PlainDescription` as
   `&#13;` and becomes a real CR, which `collapseBlankLines` does not count.

Findings 7 and 8 are out of scope by the user's selection.

## 1. One blank-line rule, in `internal/storage`

Fixes findings 1 and 6 at the root.

Two implementations of this rule exist today, and the better one is in the
higher package:

- `internal/storage`'s `collapseBlankLines` — counts `\n`, caps a run at two.
- `internal/enrich`'s `capBlankLines` (`resolver.go:128`) — folds `\r\n` and
  a lone `\r` to `\n` first, strips each line's trailing spaces and tabs
  before counting (a line of two spaces reads as blank, and `pre-line`
  collapses the spaces while keeping both newlines around them), then caps.

Move `capBlankLines`' body into `internal/storage` as the exported
`CapBlankLines`, beside `SortTitle`, `NormalizeISBN` and `PlainDescription`.
Delete `collapseBlankLines` and delete `internal/enrich`'s `capBlankLines`;
`sanitizeValue` (`resolver.go:104-109`) calls the storage one. Each has
exactly one caller today, so this is a move, not a fan-out.

Then restructure `PlainDescription` so both paths return one shape:

```go
func PlainDescription(raw string) string {
	text := raw
	if strings.ContainsAny(raw, "<&") {
		// … strip tags into b …
		text = unescapeReferences(b.String())
	}
	return trimBlank(CapBlankLines(text))
}
```

The fast path stays a fast path — it still skips the tag scan and the
unescape — but it no longer skips the shape the slow path produces. The CR
fold is what removes finding 6: a stray carriage return becomes a newline
the cap can count, rather than one it cannot see.

One behaviour change to watch in `internal/googlebooks`: its output now
gains the per-line trailing-whitespace strip. That is `capBlankLines`' own
documented reasoning applied to the other caller, and its fixtures are live
captures, so a failure there is information rather than a reason to back out.

## 2. Decode only well-formed character references

Fixes finding 3.

The review's suggested "skip the unescape when no tags were stripped" does
not work: `internal/googlebooks` has a real case with entities and no tags
(`"Salt &amp; pepper &mdash; a pair."`), pinned by the moved table test.

Require a terminating `;` instead. Replace the bare `html.UnescapeString`
call with `unescapeReferences`, which re-escapes any `&` that does not start
one and then decodes once:

```go
func unescapeReferences(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '&' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if end := referenceAt(s, i); end > 0 {
			b.WriteString(s[i:end])
			i = end
			continue
		}
		b.WriteString("&amp;")
		i++
	}
	return html.UnescapeString(b.String())
}
```

`referenceAt(s, i)` reports the index just past the `;` of a reference
starting at `s[i]` (which the caller has checked is `&`), or -1: `#` then
digits, `#x` then hex digits, or alphanumerics, at least one character, then
a `;` within `maxReferenceName` (32, the longest name HTML defines).

`Rock &copy roll` → the space breaks the run before any `;`, so the `&` is
re-escaped and comes back out as itself. `Salt &amp; pepper` and `&#8217;`
are `;`-terminated and decode as before. This deliberately does not change
`&notanentity;`, which HTML really does parse as `¬anentity;` — the finding
is about semicolon-less references in prose, not about spec-correct legacy
decoding of a terminated one.

## 3. `capValue` enforces what it claims

Fixes finding 2, in `internal/scanner/scanner.go:905`:

```go
if field != storage.FieldDescription {
	value = strings.Join(strings.Fields(value), " ")
} else {
	value = strings.TrimSpace(value)
}
```

`strings.Fields` already trims the other fields, so this makes the two
branches symmetric and the invariant self-enforcing rather than true by
luck of the callers. Blank-line capping is deliberately *not* added here —
that is the parsers' business, and a third copy of the rule is what section
1 exists to prevent.

## 4. The two small ones

- **Finding 4** — `internal/googlebooks/googlebooks_test.go:1009`: name the
  shared flattening rather than `plainText`, as the sibling comment at
  `:247` already does.
- **Finding 5** — a regression test in `internal/web/web_test.go`, in the
  shape of `TestDescriptionRendersParagraphBreaks` (`:1190`), which already
  reads `app.css` out of `staticFS` and parses its rules: assert
  `.detail__meta-row dd` carries `flex: 1`, naming in the failure message
  that without it the one non-editable row is pushed to the far edge.

## Tests

- `internal/storage/plaindescription_test.go` — add to the table: a plain
  run of five newlines caps at two; a CRLF description folds; a line of
  trailing spaces does not defeat the cap; `&copy` without a semicolon
  survives; `&amp;`, `&mdash;` and `&#8217;` still decode; `&#13;` no longer
  leaves a stray CR. Add a `CapBlankLines` table test of its own, since it is
  now exported and has two callers.
- `internal/enrich/resolver_test.go` — `TestSanitizeDescriptionCapsBlankLines`
  (`:979`) must pass unchanged against the moved function. It is the proof
  the move preserved behaviour, so do not edit it to fit.
- `internal/scanner/capmetadata_test.go` — a direct table test on `capValue`
  for the description branch: surrounding whitespace is trimmed, an interior
  break is kept. The test is in package `scanner`, so it can call the
  unexported function directly rather than routing a fixture through a scan.
- `internal/googlebooks` — existing tests must pass unchanged; the per-line
  strip is the one place to expect a surprise.

## Documentation

Current state only, replacing sentences rather than appending.

- **`CLAUDE.md`** — add `CapBlankLines` to the storage bullet's list of
  shared derivations; the enrichment invariant "A description also caps
  consecutive newlines at two" names the shared function rather than
  implying `sanitizeValue` owns the rule.
- **`docs/notes/storage.md`** — the `PlainDescription` paragraph: both paths
  return one shape, and reference decoding requires a terminating semicolon.
  Correct the sentence describing the unescape trade as being about `&amp;`
  alone. Add why `CapBlankLines` lives here, on the same argument as its
  neighbours.
- **`docs/notes/formats.md`** — the same `&amp;` correction.
- **`docs/notes/enrichment.md`** — the `capBlankLines` paragraph now
  describes a rule `internal/storage` owns and both writers share.
- **`docs/notes/scanner.md`** — the `capMetadata` paragraph's claim is now
  self-enforcing for description too; say so rather than leaving it resting
  on the parsers.

## Verification

1. `go test ./...` and `go vet ./...`. The enrich blank-line test and the
   whole `internal/googlebooks` suite passing unchanged are the two that
   matter most — they are what says the consolidation was a move.
2. `go test -race` over `internal/storage`, `internal/scanner`,
   `internal/enrich`, `internal/googlebooks` and `internal/web`.
3. Mutation-check the two new guards, as the previous change did for the
   collapse: reverting the fast-path restructure must fail the plain-text
   blank-line case, and reverting `unescapeReferences` must fail the
   `&copy` case.
4. Re-run the app against the scratchpad EPUB from the previous change,
   extended with a plain-prose description carrying a five-newline run and a
   bare `&copy`: the detail page shows at most one blank line between
   paragraphs and the ampersand intact, and `added` still sits flush-left.

---

## Correction found while implementing

Section 2 says to "re-escape any `&` that does not start one and then decode
once", which is what `unescapeReferences` does — but the plan does not say
what happens to a terminated reference that names nothing, and the obvious
reading is that it should be left alone too. It is not: `&notanentity;` is
passed through to `html.UnescapeString` and comes back `¬anentity;`, because
that is what HTML says it means. The finding was about semicolon-*less*
references in prose, and narrowing the fix to exactly that leaves spec
behaviour for a terminated one untouched. A test pins the case so the
distinction is not read as an oversight later.

Section "Documentation" also says `docs/notes/formats.md` carries a sentence
describing the unescape trade as being about `&amp;` → `&` alone, to be
corrected. No such sentence was ever written there — the previous plan
proposed it and the implementation did not carry it into the note. The
paragraph gained an accurate statement of the rule instead of a correction.
