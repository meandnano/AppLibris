# Backlog: `.button`'s base is the editors' size, and `--secondary` is a no-op

## Problem

Raised in review of PR #58, which consolidated three button systems into
one `.button`. Two related asymmetries survived that change deliberately,
because fixing either is a restyle and that PR was required to be
pixel-identical except for one foreground.

**The base carries one call site's size.** `.button` sets
`min-height: 34px; padding: 8px 14px` — the inline metadata editors'
size. The send control names `--lg` and the enrichment control names
`--md`, so two of the three call sites declare a size and the third
relies on the base being shaped for it. A new control that forgets a size
modifier silently gets the editors' size rather than failing visibly.

Neither guard in `internal/web/web_test.go` catches that, and this is the
part worth recording: `TestButtonClassesInMarkupHaveRules` asserts that
every class the markup names has a rule, and a bare `.button` *does* have
one. The omission is invisible to a test whose whole purpose is catching
the classes that go missing.

**`.button--secondary` declares nothing.** Its three properties —
`border-color: var(--rule)`, `background: transparent`,
`color: var(--fg-muted)` — are verbatim the base's, so the ruleset never
overrides anything at any call site. It predates the consolidation. It is
defensible as call-site explicitness (the templates name an intent
everywhere, and deleting it would mean "secondary" stops being a name the
stylesheet knows), which is why it stayed.

## Why this is backlog, not a plan

Nothing renders wrong today. All three call sites name their size
explicitly except the one the base is shaped for, and every one names an
intent, so the base's visual defaults are never actually observed. The
cost is entirely to the *next* control, and there is no next control
pending — `docs/plans/2026090605-empty-library-scan-action.md` adds a
primary button, which names both an intent and (whichever it picks) a
size.

It is also the same judgement PR #58's own Decision 1 made about the two
disabled opacities: a difference that renders correctly is not worth a
restyle on its own schedule.

## Re-validate before acting

- Has a fourth control landed, and did it name a size? If one forgot and
  nobody noticed, this stopped being hypothetical and the item should
  become a plan.
- Is `--secondary` still a verbatim restatement? If any of the three
  properties has since diverged, that half of this item is already gone.

## Sketch

Strip both the size and the secondary properties off the base, so every
call site must name a size *and* an intent and neither can be inherited
by accident. The editors' size becomes `--sm`.

The reason this is a sketch and not an instruction: it is not obviously
cheaper than what it replaces. `border: 1px solid var(--rule)` on the
base would have to split into `border-width`/`border-style` there plus a
`border-color` on all four intents, which is more lines and more
indirection, not less. Weigh that against what it buys — a missing size
modifier becoming visible instead of silent, and
`TestButtonClassesInMarkupHaveRules` gaining something real to guard,
since every call site would then name a modifier the stylesheet must
have.
