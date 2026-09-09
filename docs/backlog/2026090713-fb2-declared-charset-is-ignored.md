# Backlog: an FB2 file that is really Windows-1251 fails to parse at all

## Problem

`internal/fb2.readMetadata` sets a `CharsetReader` that passes every
declared encoding through unchanged:

```go
decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) {
	return input, nil
}
```

The comment beside it says this yields "a best-effort parse" of a file
whose declared encoding is wrong. It does not, and it also breaks the
file whose declared encoding is right. `encoding/xml` rejects any byte
sequence that is not valid UTF-8 with a syntax error regardless of
`Strict`, so a correctly labelled `encoding="windows-1251"` document
answers:

```
parse fb2: XML syntax error on line 1: invalid UTF-8
```

No title, no author, no cover. The book lands under its filename. And
because `maybeRegenerateCover` correctly treats a parse error as "the
question was never answered", nothing heals it on a later sweep.

Legacy Russian FB2 collections, the population DESIGN.md names as the
reason FB2 is supported at all, are predominantly cp1251, with KOI8-R a
distant second. The pass-through is right for a UTF-8 file mislabelled as
something else, which the comment describes, and wrong for every file
labelled honestly.

## Why this is backlog, not a plan

It does not corrupt data: the book is indexed, under a worse title, and
every field is hand-editable. Whether it bites depends entirely on the
library's provenance, and the one this project was built against is
UTF-8 (the comment says so). Someone whose collection is cp1251 will see
it on the first sweep, filename-titled books across the grid, and will
know what to look for here.

## Re-validate before acting

- Whether `readMetadata` still passes the charset through unchanged.
- Whether `golang.org/x/text` is still a direct dependency (it is, for
  `internal/enrich/match.go`). Its `encoding/charmap` and
  `encoding/htmlindex` packages are what the fix needs, at no new module.

## Sketch

Map the declared label to a real decoder and fall back to pass-through
only for a label nothing recognises:

```go
decoder.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(label)
	if err != nil {
		return input, nil
	}
	return transform.NewReader(input, enc.NewDecoder()), nil
}
```

`htmlindex` knows `windows-1251`, `koi8-r`, `cp1251` and the aliases FB2
tools actually write. The mislabelled-UTF-8 case the comment worried
about becomes mojibake in the title rather than a parse failure, which is
the "best effort" the comment promised and a person can fix in the editor.

Test with a live cp1251 file, not one transcoded by hand from UTF-8: the
byte order mark and the `<?xml` declaration's own encoding are where a
hand-made fixture goes wrong.
