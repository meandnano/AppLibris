# Brief: verify one rewritten note

Read-only. Do NOT edit any file.

Inputs: the checklist at `<scratch>/checklists/<name>.md`, the new note at
`docs/notes/<name>.md`, the old note via `git show <base>:docs/notes/<name>.md`
and the old CLAUDE.md via `git show <base>:CLAUDE.md`.

For every checklist line, find the bullet in the new note that states it.
Report, one line per finding with line numbers, "None" for an empty
category:

1. MISSING: checklist lines with no bullet stating the rule. Paraphrase is
   fine; a rule folded into a related bullet counts if the constraint is
   still stated.
2. WEAKENED: the new note states the rule less strictly than the sources
   ("should" for "never", a dropped condition, a dropped "both halves").
3. HISTORY: any sentence narrating a prior state rather than the present
   design.
4. UNCLEAR: a bullet whose reason no longer makes sense without deleted
   context.
5. SPOT CHECK: pick five bullets whose reason cites a code fact (a constant
   value, a function's behaviour) and confirm each against the code. Report
   any that are wrong, including ones inherited from the old note.
