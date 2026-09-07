# Step: one button system, and the dark-theme contrast bug it is hiding

## Position in the sequence

Independent, and worth building **before**
`docs/plans/2026090605-empty-library-scan-action.md` if the two are done
close together: that step adds a primary button to the empty-library state
and would otherwise have to pick one of three systems arbitrarily, which is
the exact failure this step exists to stop.

## Context

`docs/backlog/2026090301-button-style-duplication.md`, re-validated — and
the prediction in it has already come true. The item describes **two**
near-identical button systems. There are now **three**:

| rule | added with | padding | `--primary` foreground |
|---|---|---|---|
| `.send__button` | the send control | `11px 20px` | `#fff` |
| `.button` | the inline metadata editors | `8px 14px` | `var(--bg-raised)` |
| `.enrich__button` | the enrichment control | `9px 16px` | *(no primary variant)* |

All three set `font-family: var(--mono)`, `font-size: 11px`,
`letter-spacing: 0.1em`, `text-transform: uppercase`,
`display: inline-flex`, `align-items: center`, `gap: 9px` and a 1px border.
They differ in padding, in the border and background defaults, and in that
one foreground.

`.send__spinner` and `.enrich__spinner` are the same story one scale down:
identical `border`, `border-top-color`, `border-radius` and
`animation: search-spin 0.7s linear infinite`, differing only in being
11px and 10px square.

The item's reasoning for deferring it was sound — collapsing them meant
restyling a control that had just shipped after manual browser
verification, and deduplicating CSS is not worth risking a regression in a
feature verified by eye rather than by test. What has changed is that a
third system appeared in the meantime, exactly as predicted, and that the
duplication turns out to be **concealing a real defect**.

## The defect

`.send__button--primary` hardcodes `color: #fff` on a `var(--accent)`
background. `.button--primary` uses `color: var(--bg-raised)` on the same
background. Those are the same thing in light and opposite in dark:

| | `--accent` | `--bg-raised` | `#fff` on accent | `--bg-raised` on accent |
|---|---|---|---|---|
| light | `#8a5a3c` | `#ffffff` | ~7.0:1 ✓ | ~7.0:1 ✓ |
| dark | `#c98a5f` | `#1a1817` | **~2.9:1 ✗** | ~6.1:1 ✓ |

In dark theme `--accent` is a light tan and `#fff` is white on it. The
send control's primary button — **the primary action of the whole
application**, per DESIGN.md's first paragraph — is below the 4.5:1 minimum
in dark theme, and below even the 3:1 large-text floor.

`.button--primary`, written later, got it right by using the token. So this
is not "two ways of saying the same thing"; it is one correct
implementation and one that is wrong half the time, and the reason it went
unnoticed is that the send control was verified in a browser before
`.button` existed to compare it against.

That converts this from tidying into a fix, and it is why the plan leads
with the contrast rather than the duplication.

## Scope

In scope: one `.button` base with size and intent modifiers, all three
call sites moved onto it, one `.spinner` with a size modifier, and the
`#fff` foreground gone.

Out of scope, with reasons:

- **Restyling anything.** The output should be pixel-identical to today
  everywhere except the dark-theme primary foreground, which is the fix.
  Any other visual change makes the before/after comparison — the only
  verification this step has — useless.
- **A token for button padding.** Three sizes is three values; naming them
  in `:root` adds indirection to a stylesheet nobody is generating.
- **Auditing contrast elsewhere.** `--fg-muted` on `--bg-sunken`, the
  status colours, the `--fg-faint` hint lines: all worth checking, none of
  it this step. A contrast pass over the whole palette is its own piece of
  work and would bury this one.
- **Touching the search control or the paging trigger.** Neither uses a
  button system; the reveal trigger is an `<li>` and the search affordances
  are links.

## Decision 1: `.button` is the base, and the tokens win over the literals

`.button` is already the most nearly-correct of the three — it is the one
that uses `var(--bg-raised)`, the one that already undoes link decoration
(Cancel is an `<a>`), and the one whose comment already anticipates this
step. Promote it.

```css
.button        /* everything shared, at the editors' current size */
.button--lg    /* padding: 11px 20px  — the send control */
.button--md    /* padding: 9px 16px   — the enrichment control */
.button--primary   /* accent ground, var(--bg-raised) text */
.button--secondary /* transparent, --rule border, --fg-muted text */
```

`--bg-raised` is the correct foreground in both themes, per the table
above, and it is correct *because it is a token* — it tracks the theme,
where `#fff` states a light-theme fact. Delete `#fff`.

The enrichment button's default (a `--bg-raised` ground and `--fg` text,
rather than transparent and `--fg-muted`) is a third intent, not a third
system. Whether it becomes `.button--tertiary` or the base default with
the other two overriding is an implementation detail; what matters is that
`.enrich__button`'s hover rule (`border-color`/`color` to `--accent`) and
its `:disabled` rule (`opacity: 0.55`) survive, since the disabled
treatment is what "no provider configured" renders and it is more subdued
than `.send__button:disabled`'s `0.85` on purpose.

**Correction, found while implementing.** The clause above starting "since
the disabled treatment" is wrong, and the reason for keeping `0.55` is not
the reason given. "No provider configured" renders
`<p class="enrich__disabled">` and no button at all — the whole `<form>` is
in the `{{else}}` branch. The enrichment button carries `disabled` only
under `{{if .EnrichPending}}`, and `EnrichPending` is set for `queued` and
`running` alone, which is exactly the shape `SendPending` has for `queued`
and `sending`. So both buttons are disabled for one reason each, the same
reason — their own job is in flight — and `0.55` against `0.85` is an
unexamined divergence between two controls rather than a considered
contrast between a standing condition and a wait.

The instruction survives its own justification: both values are still kept,
because picking one would be a restyle and this step is pixel-identical
except for the primary foreground. Only the stated reason changes, and the
CSS comment states the corrected one.

## Decision 2: `.spinner` with a size modifier

`.send__spinner` and `.enrich__spinner` collapse to `.spinner` plus
`.spinner--sm`, keeping the shared `animation: search-spin …`. The
animation name is already shared with the search control, which is the
existing precedent for a cross-component primitive here.

## Decision 3: the class names in the templates change, and that is the risk

Three template files reference these classes:

- `partials.html:203` — `send__button {{if .SendButtonPrimary}}send__button--primary{{else}}send__button--secondary{{end}}`
- `partials.html:289` — `enrich__button`
- `partials.html:325–326` — `button button--primary`, `button button--secondary`

The conditional at 203 is the one to be careful with: it is inside the
send control's state machine, so getting it wrong shows up only in whichever
of the five states is not the one being looked at. Keep the conditional's
shape and change only the class names, so the diff is a rename rather than
a rewrite.

An alternative that avoids touching templates at all — keeping the old
selectors as aliases (`.send__button, .button { … }`) — is rejected: it
leaves three names for one thing in the stylesheet, which is the state this
step exists to end, and it would make the next control's author face the
same arbitrary choice.

## Changes

`internal/web/static/css/app.css`:

- One `.button` block with the shared properties, plus `--lg`, `--md`,
  `--primary`, `--secondary`, hover and `:disabled` rules.
- `.send__button*` and `.enrich__button*` blocks deleted.
- `.spinner` / `.spinner--sm`; `.send__spinner` and `.enrich__spinner`
  deleted.
- The comment above `.button` explaining that it deliberately absorbed the
  send and enrichment buttons, and why `--primary`'s foreground is a token
  rather than `#fff` — that literal is the one thing here somebody would
  reintroduce as an "obvious" simplification.

`internal/web/templates/partials.html`: the four class attributes above.

No Go change.

## Tests

CSS is not unit-testable here and pretending otherwise would add a test
that asserts a string. What is worth having:

- An assertion that `partials.html` contains no `send__button`,
  `enrich__button`, `send__spinner` or `enrich__spinner` — cheap, and it
  is what catches a half-finished rename, which is the realistic failure.
- The existing `internal/web` tests over the send and enrichment control
  states keep passing; they assert on structure and copy, not classes, so
  they should be untouched. If any of them do assert a class name, that is
  the list of places to update, and it is worth checking before starting.

The real verification is below and is manual, which this plan states
plainly rather than dressing up.

## CLAUDE.md

`internal/web`'s paragraphs mention neither button system, so nothing
there is now wrong. Worth one clause somewhere in the CSS discussion:
there is one button system, `.button`, and `--primary`'s foreground is
`var(--bg-raised)` because `#fff` on `--accent` fails contrast in dark
theme — a fact whose only durable home is a comment and a note, since the
palette makes it non-obvious in light theme where it looks fine.

## DESIGN.md (on `init`)

No change. It does not describe component-level CSS.

## Verification

Manual, in a browser, **in both themes** — the whole point is that one
theme looked fine throughout. Every state of every control that has a
button:

- **Send control**, all five: idle with saved recipients, idle with none
  (the `+ add address` `<details>` open), sending, delivered, failed —
  plus the disabled state with `RESEND_API_KEY` unset, and the rejected-
  address state that re-renders with the typed values.
- **Enrichment control**: idle, pending (spinner), "Nothing to add",
  "Added …", failed, and the disabled state with `METADATA_PROVIDERS=`.
- **Inline editors**: Save and Cancel, on a text field and on the
  description textarea, and the 422 rejected state.
- **The recipient remove buttons** in the address list.

For each: compare against a screenshot taken before the change. Everything
must be identical except the dark-theme primary send button's text, which
should go from white to near-black on the tan accent — and which should be
checked with a contrast tool rather than by eye, since the failing version
is the one that looks "brighter".
