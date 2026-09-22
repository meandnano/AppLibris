---
name: compact-docs
description: Compact the agent-facing documentation (CLAUDE.md, docs/notes/, README.md) back to rules with one reason each, no history, no essays. Use when asked to "compact the docs", "slim down CLAUDE.md", "the notes have grown again", "trim the documentation", or after a run of features has left the notes chatty.
argument-hint: "[all | claude | notes | readme | <note-name>...]"
---

# Compact docs

Periodic compaction of the three agent-facing documents. The target shape is
fixed by earlier decisions (below); the work is: extract every rule into a
checklist, rewrite each file against it, verify nothing was lost, commit per
file.

## Settled decisions

These were decided once and hold for every run. Do not re-ask them.

- **Invariants live in the notes, not in CLAUDE.md.** CLAUDE.md carries the
  code map (one line of ownership plus a note pointer per package), the
  cross-cutting conventions, and the Documentation, Planning and Backlog
  process sections. A rule that belongs to one area belongs in that area's
  note. If a rule has crept into CLAUDE.md since the last run, move it.
- **A note is topic headings over bullets of the form bold rule plus one
  reason.** Rule: imperative or declarative, at most two lines. Reason: the
  single decisive one, present tense, one or two sentences. Worked examples,
  restated consequences, second reasons and rhetorical framing are deleted.
  A rule with no reason in any source stays as a bare bold sentence and is
  listed in the final report for the user to judge; it is never dropped.
- **Every rule survives.** Length is a consequence, not a target. Expect
  notes to land around 60% of an essay-style version; the floor is the rule
  count. Cutting below that means dropping rules, which is the user's call.
- **No history.** Present tense only. A prior state appears only as a
  present-tense contrast that explains the current design. Banned: "used
  to", "no longer" (as history), "first built as", "the plan said", "was
  corrected", "before this change", "previously", "originally", "review
  found", "was changed", "has been moved/removed/replaced". No plan, PR or
  commit citations; a `docs/backlog/` file may be cited for a known limit.
- **Process sections stay.** Documentation, Planning and Backlog in
  CLAUDE.md are edited only where a sentence is redundant. They are
  followed strictly and are already rule-shaped.
- **README is a wording pass only.** Every fact, command and the
  configuration table stay word-for-word in meaning; headings and order
  are unchanged. Cut phrasing, never content.
- **Wording.** Short sentences. No em-dashes in prose (the code map's
  `` `pkg` — description `` separator is the one exception). No
  parentheticals except to attach a value or file to an identifier. Code
  identifiers in backticks only where the reader must go there, and every
  one must resolve in the code today. Headings never end with a dot.
- **Out of scope.** `docs/plans/`, `docs/backlog/`, code comments.

## When to ask

Ask the user, one question at a time with `AskUserQuestion`, only when a
decision is not covered above and different answers lead to different
work. Examples that warrant a question: a note has grown so large it should
split in two; a rule in a note contradicts the code and it is unclear which
is right; a whole section describes something the code no longer has; the
user's argument names a file that does not exist. Do not ask about anything
in Settled decisions, and do not ask for permission to proceed.

## Process

Work on a branch. One commit per file, no push. Never add a
`Co-Authored-By` trailer. In the briefs, `<scratch>` is the session's
scratchpad directory and `<base>` the commit the branch started from.

### 1. Scope and measure

Parse the argument: `all` (default) covers CLAUDE.md, every note and
README; `claude`, `notes`, `readme` or note names narrow it. Record
`wc -w` for every file in scope. Run the history grep from step 4 once now
to see how much has crept in.

### 2. Rewrite each note

One subagent per note, in parallel, each given
`references/rewrite-brief.md` and its file name. The brief makes the agent
first write a checklist (one line per distinct rule, tagged by source) to a
scratch directory, then rewrite the note against it, then report word
counts, unplaced lines, reasonless rules, unresolved identifiers and
judgement calls. Read every report; a report claiming a line could not be
placed is a stop, not a note.

Sources per note: the note itself, and any rule about its area that has
appeared in CLAUDE.md's code map or conventions since the last run.

### 3. Rewrite CLAUDE.md and README

After the notes, since the code map's pointers depend on final notes.
CLAUDE.md: code map to one line per package plus pointer, conventions kept
and tightened, process sections touched only for redundancy. README: the
configuration table is spliced back verbatim from the original; everything
else is reworded.

### 4. Verify

Mechanical, over every file in scope:

```sh
grep -nEi "used to|first built|the plan said|was corrected|before this change|previously|originally|review found|was changed|has been (moved|removed|replaced)" CLAUDE.md README.md docs/notes/*.md
grep -n "—" docs/notes/*.md          # only a delimiter literal may remain
grep -n "CLAUDE.md" docs/notes/*.md  # notes never describe CLAUDE.md
grep -nE "^#.*\.$" docs/notes/*.md   # headings ending with a dot
grep -nE "docs/plans|PR #" docs/notes/*.md
```

Then resolve every backticked identifier in the notes against the code
(`references/check-identifiers.sh`). External names such as syscalls,
inotify flags and htmx patterns are expected non-hits; anything else is a
stale name to fix.

Then independent review: one subagent per note, given
`references/verify-brief.md`, which compares the new note to the checklist
and reports MISSING, WEAKENED, HISTORY, UNCLEAR and a five-item SPOT CHECK
of code facts. Fix every MISSING and WEAKENED finding and every wrong spot
check before committing. HISTORY and UNCLEAR findings are fixed unless the
verifier itself marks them borderline and the sentence reads as present
tense.

### 5. Commit and report

One commit per note, then CLAUDE.md, then README. The final report gives
the before and after word counts in a table, the rules left without a
reason, any factual error found in the old text and fixed, and any place
the length floor stopped further cutting.
