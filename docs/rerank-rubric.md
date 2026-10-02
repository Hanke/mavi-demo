# Rerank rubric

Version 1. This is what `/rerank` scores a candidate on, and how the score of
a match is arrived at. The model never produces that score. It places the
candidate on a written level for each of five dimensions, with a sentence of
evidence and quotes from the candidate's text for each, and the service
computes the score from the levels.

The rubric lives in [`ai/app/rubric.py`](../ai/app/rubric.py). The tables
below are rendered from it, the `/rerank` system prompt is built from the same
text, and the response schema has one field per dimension, so the three cannot
disagree (`ai/tests/test_rubric.py` checks each). To change the rubric, edit
that module, bump `VERSION`, and run `make rubric-render generate`.

## What is scored

The rerank sees candidates that already passed the hard filters
(certifications, required software, minimum years, and the start date, hours
a week and time-zone overlap the candidate supplied; see the README). It reads the role and each candidate's text and judges what a filter
cannot: the must-haves that are only prose, how deep the experience is, and
whether a named tool was actually used.

Each dimension asks one question, so that one fact is not counted twice:
presence of a required tool is must-have coverage, how well the candidate
knows it is software fluency; a qualification from another country is settled
in must-have coverage, experience under the role's accounting framework is
part of experience depth.

## Scale

Every dimension is scored on the same five levels, 0 to 4, each with its own
written description below. The model picks the level whose description fits
best: there are no half levels. Only what the candidate's text shows counts; a
claim with no supporting detail is "listed", not "evidenced", and something
the text does not mention is not met.

A dimension the role gives nothing to score against is `null`, not 0: a JD
with no nice-to-haves cannot be failed on them. That is decided by the role,
so a dimension is null for every candidate in a request or for none (a
response that mixes the two is sent back to the model). Experience depth is
always scored.

## Evidence

A level of 1 or higher comes with one to three quotes: short passages copied
from the candidate's text. The service looks each one up in that candidate's
text and returns it as the text writes it, so a quote in a response is always
a literal substring of what was judged. A quote that is not there is sent back
to the model once and dropped if it is still not there; the level is the
model's judgement and is left alone, so a scored dimension with no quotes is
one the model could not back.

<!-- rubric:begin (rendered from ai/app/rubric.py by `make rubric-render`; do not edit) -->
| Dimension | Key | Weight | Null when |
| --- | --- | --- | --- |
| Must-have coverage | `must_have_coverage` | 40% | the role states no must-haves |
| Relevant experience depth | `experience_depth` | 25% | never: always scored |
| Software fluency | `software_fluency` | 15% | the role names no software |
| Industry fit | `industry_fit` | 10% | the role names no industry |
| Nice-to-haves | `nice_to_haves` | 10% | the role states no nice-to-haves |

### Must-have coverage (`must_have_coverage`, 40%)

Does the candidate meet each thing the role states as required: the qualification, the years, the required software, and any other must-have? Met or not met, not how well.

| Level | The text shows |
| --- | --- |
| 4 (full) | Every must-have is met, and the text shows each one. |
| 3 (strong) | All but one must-have is met, and the one missing is not the required qualification: a near miss (4 years where 5 are asked, a direct counterpart of a required tool) or something the text does not mention. |
| 2 (partial) | Most must-haves are met, but the required qualification is missing or two or more others are. |
| 1 (weak) | Fewer than half of the must-haves are met. |
| 0 (none) | No must-have is met, or the candidate works in a different field. |

### Relevant experience depth (`experience_depth`, 25%)

How closely does the work the candidate has actually done match the work of this role: the same function, at the same seniority and scope, recently, under the same accounting framework?

| Level | The text shows |
| --- | --- |
| 4 (full) | Has done this job: the same function and seniority at a comparable scope (entities, team, volume), within the last three years, under the same accounting framework. |
| 3 (strong) | The same function with one difference: a level more junior or more senior, a clearly smaller scope, a different accounting framework (IFRS or UK GAAP for a US GAAP role), or not within the last three years. |
| 2 (partial) | An adjacent function (accounts payable for a staff accountant role, audit for a controller role), or the right function with two or more of the differences listed under level 3. |
| 1 (weak) | Finance or accounting experience with little bearing on the work of this role. |
| 0 (none) | No finance or accounting experience. |

### Software fluency (`software_fluency`, 15%)

How well does the candidate know the systems the role names, required or preferred? Depth of use, not presence: a product that only appears in a list is listed, not evidenced.

| Level | The text shows |
| --- | --- |
| 4 (full) | Evidenced hands-on use of every system the role names, with specifics: administered, implemented, migrated, or used for the role's core work. |
| 3 (strong) | Evidenced hands-on use of the role's main system; the others are only listed, or covered by a direct counterpart. |
| 2 (partial) | The role's main system is only listed with no evidence of use, or the evidenced use is of a direct counterpart (Sage Intacct for NetSuite, Xero for QuickBooks). |
| 1 (weak) | Only unrelated finance systems, or only spreadsheets. |
| 0 (none) | No software is named at all. |

### Industry fit (`industry_fit`, 10%)

Has the candidate worked in the industry the role is in?

| Level | The text shows |
| --- | --- |
| 4 (full) | The current or most recent role is in the role's industry, with three or more years in it. |
| 3 (strong) | Has worked in the role's industry, but not in the current or most recent role, or for under three years. |
| 2 (partial) | An adjacent industry whose accounting works the same way (another subscription business for SaaS, another inventory business for manufacturing), or clients in the role's industry served from a practice or an outsourced team. |
| 1 (weak) | General exposure only: mixed clients or industries, none of them the role's or an adjacent one. |
| 0 (none) | Only unrelated industries, or the text does not show any. |

### Nice-to-haves (`nice_to_haves`, 10%)

How many of the things the role states as preferred, a bonus or nice to have does the text show?

| Level | The text shows |
| --- | --- |
| 4 (full) | All of them. |
| 3 (strong) | More than half. |
| 2 (partial) | At least one, and no more than half. |
| 1 (weak) | None, but something close: studying for a preferred certification, a direct counterpart of a preferred tool. |
| 0 (none) | None, and nothing close. |
<!-- rubric:end -->

## Overall score

Computed by `rubric.overall` from the levels, in three steps:

1. **Weighted mean.** Over the dimensions that are not null, with weight
   `w` and level `l`: `score = Σ(w × l) / (4 × Σw)`. Dropping a null
   dimension and dividing by the remaining weight is the same as scaling the
   other weights up to 100.
2. **Must-have cap.** A must-have is not one consideration among five. If
   must-have coverage is 0 the score is at most 0.30, and if it is 1 at most
   0.50, however strong the rest is. No cap applies from level 2 up, or when
   the dimension is null.
3. **Rounding** to three decimals.

The result is between 0 and 1. Results are returned best first; candidates
held at the same cap are ordered by their score before the cap, and candidates
who are still equal by id, so the same levels always give the same order.

Worked examples:

| Must-have | Experience | Software | Industry | Nice-to-have | Arithmetic | Score |
| --- | --- | --- | --- | --- | --- | --- |
| 3 | 3 | 2 | 4 | null | (40×3 + 25×3 + 15×2 + 10×4) / (4 × 90) = 265 / 360 | 0.736 |
| 4 | 4 | 4 | 4 | 4 | 400 / 400 | 1.000 |
| 1 | 4 | 4 | 4 | 4 | 280 / 400 = 0.700, capped | 0.500 |
| null | 2 | null | 3 | null | (25×2 + 10×3) / (4 × 35) = 80 / 140 | 0.571 |

Each result carries its `dimensions` (level, evidence and quotes) next to the
`score`, and the response carries `rubric_version`, so the score of a stored
match can be recomputed from its breakdown and traced to the weights that
produced it.

## Why these choices

- **Must-haves weigh most (40%) and cap the score.** The employer called them
  requirements. Without the cap a candidate who meets none of them but is
  strong elsewhere would score 0.60 and outrank one who meets them all with
  middling depth.
- **Experience depth is second (25%)** because it is what separates the
  candidates who all pass the filters, and it is where the accounting
  framework question goes (an ACA who has only reported under FRS 102 meets a
  "CPA or equivalent" must-have and loses a level here for a US GAAP role).
- **Software fluency (15%) is separate from the must-have** because the hard
  filter and the must-have level only establish that the product is named.
- **Industry fit and nice-to-haves (10% each)** move a candidate within a
  band; neither can lift one past a missing must-have.
- **Integer levels with written descriptions**, rather than a 0..1 number per
  dimension, because two runs (or two models) can be held to "which
  description fits" in a way they cannot be held to 0.7 versus 0.75.
- **The weights are a starting point**, not measured. `make eval` reports how
  the ranking does on the fixtures; revisit the weights when there is review
  data from ops (approve / reject / swap) to fit them to.
