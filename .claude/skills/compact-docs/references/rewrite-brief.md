# Brief: rewrite one note

You rewrite exactly ONE file, named in your task. Do not touch any other
file. Do not commit.

## Goal

The note is being compacted to rules with one reason each. Every rule
survives; narrative, worked examples, restated consequences, second reasons
and rhetorical framing go. Any rule about this note's area that sits in
CLAUDE.md (code map or conventions) moves in here.

## Inputs

1. The current note.
2. CLAUDE.md: the code map bullet(s) for the packages this note covers, and
   any convention about this area. A bullet there may state a rule or a
   design fact the code does not show (an ordering that must hold, a thing
   that must never be added). Absorb those; ignore what a reader can see by
   opening the code.

## Step 1: checklist

Write `<scratch>/checklists/<note-name>.md`, one line per DISTINCT rule or
design decision found in the inputs:
`- [src] rule in one line`, src = `C` (CLAUDE.md only), `N` (note only),
`CN` (both). Deduplicate. A reason is not a rule; it attaches to its rule.
Be exhaustive: a verifier compares the rewritten note against this file.

## Step 2: rewrite the note in place

```
# <Area>

One sentence naming the package(s) this note governs.

## <Topic>

- **Rule.** Reason in one or two sentences, present tense.
- **Rule.** Reason.
```

- Topics are the note's existing subject groupings, in the existing order
  where sensible, merged where two headings say one thing.
- Every checklist line becomes exactly one bullet, or is folded into a
  bullet carrying a tightly related rule. Never dropped.
- Rule = the "what", bold, at most two lines. Reason = the one decisive
  "why". If the old text gives several, keep the one the rule cannot be
  understood without.
- A rule with NO reason in either input stays as a bare bold sentence. List
  these in your report.
- Prose that is neither rule nor reason is deleted.
- A deferred or not-built list stays, as bullets.

Wording rules, strict:

- Present tense only. Never: "used to", "no longer" (as history), "first
  built as", "the plan said", "was corrected", "before this change",
  "previously", "originally", "review found", "was changed", "has been
  moved/removed/replaced". A prior state appears only as a present-tense
  contrast that explains the current design.
- No plan, PR or commit citations. A `docs/backlog/` file may be cited for a
  current known limit.
- Short sentences. No em-dashes. No parentheticals except to attach a value
  or file to an identifier.
- Code identifiers in backticks only where the reader must go there. Every
  identifier must exist in the code now: check with `grep -rw` over the
  source tree before keeping one; drop or rename a stale one and report it.
- Headings never end with a dot. No emojis.
- Cross-references to other notes as plain file names.
- Never describe what CLAUDE.md contains; it points here, not the reverse.

## Step 3: report

1. Word count before and after.
2. Checklist lines you could not place (should be none).
3. Rules left without a reason.
4. Identifiers that did not resolve and what you did.
5. Judgement calls between rule and narration, one line each.
