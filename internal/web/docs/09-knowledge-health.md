# Knowledge health (keeping the vault trustworthy)

A knowledge base is only useful if you can trust it. Mesh runs a lifecycle check that
flags notes which have gone stale or wrong, so the vault self-heals instead of rotting.
Run it with `mesh health`, the `mesh_health` MCP tool, or read the counts on the
Dashboard.

## What it checks

- **Dead references.** A note that cites a source file which no longer exists in the
  code index (it moved or was deleted). Mesh only flags a path inside a directory it
  actually indexes, so cross-repo or illustrative filenames never cry wolf.
- **Overdue reviews.** A note with a `review_by` date in the past. Time-sensitive
  knowledge (a "current" status, a temporary workaround) can carry a review date so it
  is flagged for re-checking instead of silently aging. Use `YYYY-MM-DD` for a
  deadline through the end of that UTC day, or an RFC3339 timestamp with an explicit
  offset for an exact instant. Health checks and optional freshness ranking share
  this rule. Empty, invalid or event-based text is not a scheduled deadline;
  absence of an overdue finding is not evidence that the guidance is current.
- **Contradictions.** Explicit recommendations and prohibitions in decisions,
  remedies, methods and procedures are compared when notes share a tag. A
  conservative heuristic flags possible disagreement for review; descriptions of
  impacts, causes and examples do not become recommendations. Historical shorthand
  is read through the legacy adapter. A flag is not proof of a factual conflict.

Findings are written to the index and surfaced as counts on the Dashboard and in full
by the tool, grouped by issue. Fixing or updating the flagged note clears it on the
next check.

If the health check reports that it cannot read the index, the index was written by an
older Mesh and the version you upgraded to reads a different shape. No read-only surface
can migrate it, so rebuild it once with `mesh index <vault>`. A clean bill of health is
never reported from an index Mesh cannot read: not knowing and finding nothing are
opposite answers, and only one of them is good news.

## Freshness in ranking (optional)

Retrieval can apply a gentle freshness decay so an equal-but-stale note ranks below a
fresh one. It is type-aware: institutional memory (decisions, gotchas, post-mortems)
decays slowly or not at all, while a `note` or `status` decays on a configurable
half-life. It is off by default (Settings: `freshness_half_life_days`), so nothing
changes silently.

## The point

Health is what lets the flywheel compound without turning into a junk drawer. An agent
that writes back also gets nudged, via `mesh_health`, to fix what its change made stale,
so the vault stays a thing you can rely on rather than a pile that grows.
