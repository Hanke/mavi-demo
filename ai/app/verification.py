"""Where a registry check of a qualification would plug in. It does not exist.

Nothing in the demo confirms that a candidate really holds the ACA or CPA
their resume claims: the candidates are synthetic, so there is nobody to look
up in the ICAEW member directory or a US state board's licensee search. The
interface is here so the match pipeline has one place to call when a real
registry client is written; until then every answer is "unverified".
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Literal, Protocol

from app.schemas import Qualification

Outcome = Literal["verified", "not_found", "unverified"]


@dataclass(frozen=True)
class Verification:
    outcome: Outcome
    source: str | None = None
    """The registry consulted, e.g. "ICAEW member directory"; None when none was."""
    detail: str | None = None


class QualificationVerifier(Protocol):
    def verify(self, full_name: str, qualification: Qualification) -> Verification: ...


class NoRegistry:
    """The only implementation: looks nothing up."""

    def verify(self, full_name: str, qualification: Qualification) -> Verification:
        return Verification(outcome="unverified", detail="registry lookups are not implemented in the demo")
