# Backlog: a provider's `language` can be wrong on a volume it matched correctly

## Problem

`enrich.Resolve`'s search fallback merges `language` like any other
field. `2026090602` added `plausibleMatch` over that path and withholds
only `isbn`, so a search answer that passes the gate writes
`books.language` — and `enrich.Resolve`'s `isMissing` asks for a field
only when it is empty and not `manual`, so a wrong value is never
reconsidered.

The gate compares title and author. Neither carries the language, so a
catalogue that has correctly identified a volume and mislabelled its
language passes unchallenged. One captured instance, from the live Google
Books check (`internal/googlebooks/testdata/volumes_wrong_edition.json`):

```
q=intitle:"O Alquimista" inauthor:"Paulo Coelho"
  -> title "O Alquimista", authors ["Paulo Coelho"], language "en"
```

A thin OCLC-derived stub — a lone `OCLC:1055765354` identifier, no
publisher, no description — that genuinely is that title by that author,
and says `en` for a Portuguese one.

The same shape is reachable through `internal/openlibrary`. This is not
a Google Books defect and not fixable in a provider client.

## Why this is backlog, not a plan

**Because the obvious fix is measurably worse than the problem.**

The tempting remedy is to withhold `language` from a search answer the
way `isbn` is withheld — one `delete(missing, storage.FieldLanguage)`
beside the existing line. Run over the population the search fallback
exists for (Russian editions with no ISBN), the shipped gate gives:

| query | answer | gate | language |
|---|---|---|---|
| Пикник на обочине / Аркадий Стругацкий | Пикник на обочине | accepted | `ru` ✓ |
| Хоббит / Джон Толкин | Хоббит, или Туда и Обратно | accepted | `ru` ✓ |
| Мастер и Маргарита | Мастер и Маргарита | accepted | `ru` ✓ |
| Преступление и наказание / Фёдор Достоевский | *Prestuplenie i nakazanie (SokraScënnoe izdanie)* | **rejected** | — |
| O Alquimista / Paulo Coelho | O Alquimista | accepted | `en` ✗ |
| Roadside Picnic / Arkady Strugatsky | Roadside Picnic (Hachette, 2014) | accepted | `en` ✓ |

Four correct, one wrong. Withholding sacrifices the four to avoid the
one. The gate already refuses the genuine cross-language mismatch — a
transliterated German abridgement, refused as `title_mismatch`, because
transliteration *is* a title mismatch — so what withholding would buy is
only the case no gate can reach.

`2026090602` declined to withhold `published_date` on the reasoning that
a field nothing keys off, visible on the detail page and hand-fixable is
not worth hedging the gate against. `language` measures out the same way,
now with numbers rather than an argument.

It does not corrupt data in the sense that bar means: it is one visible,
editable field, occasionally wrong because a third party's catalogue is
wrong, which is inherent to enrichment rather than a defect in how this
project consumes it. Editing it marks the field `manual`, and no later
run overwrites that.

## Re-validate before acting

- Whether `Resolve` still withholds only `isbn` on the search route.
- Whether the counts above still hold. Six queries is a small sample
  chosen from one library's shape; a wider one could move the trade. If
  wrong writes come to outnumber correct ones, withholding becomes right
  and this becomes a one-line plan.

## Sketch, if it ever is worth acting on

Not withholding. Two options that keep the correct values:

- **Script agreement.** Compare the script of the book's title against
  the candidate's `language` — a Cyrillic title answered `en` is
  suspicious in a way a Latin one is not. Catches the cross-script case
  the gate already catches by another route, and misses *O Alquimista*,
  which is Latin script. Probably not worth it on its own.
- **Provenance the reader can see.** The detail page already renders a
  provider marker for a value a provider supplied
  (`providerSourceNote`), and `language` carries one. So the wrong value
  is already labelled as a third party's guess at the point someone
  would notice it. Arguably the feature is doing what it should, and the
  right answer here is to do nothing at all.
