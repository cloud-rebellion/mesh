# The flywheel (how Mesh gets smarter with use)

The flywheel is the one thing about Mesh that compounds: agents write back what they
learn, the next agent inherits it, and the vault gets more useful every session. This
page is how Mesh feeds, measures, and proves that loop.

## The loop

1. An agent does work and learns something useful: a finding, decision, procedure or
   incident account, with evidence and the limits of what was verified.
2. It selects a purpose-specific template and calls `mesh_author_note` with a summary
   and authored sections. Complete notes publish normally; incomplete work stays in
   the draft inbox. Mesh derives identity, timestamps and placement.
3. The next agent, on its next session, retrieves that note and starts smarter.

The session hooks (`mesh hooks install`) make this automatic: read the mesh at the
start of every session, get nudged to write back before finishing.

## Measuring it (the Dashboard)

A flywheel only matters if written notes actually get reused, so Mesh measures it
instead of asserting it. The Dashboard shows:

- **Reuse rate.** Of the notes that were written back, how many were fetched again in
  a LATER session (a fetch at least ten minutes after a note was authored counts as
  cross-session reuse, which works for both a solo CLI and a long-lived team hub).
- **Time to first reuse.** The median lag from writing a note to it being used again.
- **Write-back input health.** Write-backs per 100 reads, so you can see whether the
  loop is being fed or starved.

The number is honest and per-vault. It reflects your accumulated agent-authored notes
from day one (Mesh seeds the measurement from the existing corpus), and it climbs as
those notes get reused going forward.

## Feeding it automatically (the Review queue)

Most sessions end without the agent writing anything back, even with the nudge. So Mesh
can pull the durable learnings out of a finished session for you. When a session ends
with no write-back, an opt-in Stop hook (`mesh hooks install --extract`) extracts a few
candidate notes from the transcript using your own LLM and puts them in the **Review**
tab.

Open a candidate, resolve uncertainty and fill the selected template's required
sections. **Publish note** validates the content and supported references, then saves
it through the shared writer; **Discard** removes the candidate. The queue entry stays
until publication succeeds. The receipt distinguishes a saved note from a pending
index update. Historical candidates keep their original wording for a deliberate
rewrite rather than being silently assigned new meanings.

This queue helps inspect transcript-derived candidates. It is not an approval gate
for ordinary complete notes using an available template. New or changed library
templates and blocks require approval. With configured hosted folder rules,
queue publication currently fails closed because it cannot prove that the destination
has the same audience; the candidate remains queued. Identified members can publish
when no folder rules exist. An unrestricted shared-token callback alone cannot
establish that absence.

Two honest properties of auto-extraction:

- Transcript extraction can mistake a local observation for a general rule. Keep the
  source and confidence visible, and leave unsupported facts incomplete. The judge
  corpus now uses purpose-specific prose; historical benchmark results need a new
  baseline before they support claims about this authoring format.
- Candidates that merely restate a note you already have are filtered out (deduped
  against the vault), so the queue shows new knowledge, not echoes.

You can also run it by hand: `mesh extract <transcript>` to preview, or
`mesh extract --recurring <dir>` to find problems that recur across many sessions (a
systemic issue worth a permanent fix, not just another note).
