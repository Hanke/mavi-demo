"""The deterministic half of the seed: who the ~200 candidates are.

Everything that the hard filters and the ranking key on (certifications,
software, availability, timezone, years, industries) is decided here with a
seeded RNG, so the distribution is reviewable in code and stable across
regenerations. The model only writes prose for the spec it is handed.
"""

from __future__ import annotations

import random
from dataclasses import asdict, dataclass, field, replace
from typing import Any

from app import taxonomy

PLAN_SEED = 20260930
DEFAULT_COUNT = 200

# Candidate ids continue the original hand-written seed's prefix so the README
# examples (candidate ...0001) keep working.
ID_PREFIX = "11111111-0000-0000-0000-"

Availability = str


@dataclass(frozen=True)
class Slot:
    """One planned candidate: the spec the model writes a resume for."""

    index: int
    id: str
    full_name: str
    email: str
    phone: str
    location: str
    timezone: str
    source: str
    status: str
    archetype: str
    title: str
    years_experience: int
    certifications: list[str]
    software: list[str]
    industries: list[str]
    gaap: list[str]
    availability: Availability
    available_in_days: int | None
    # What the candidate supplies after uploading (candidate_availability):
    # working hours local to `timezone`, and hours a week. All None, with
    # available_in_days, for a candidate who has not answered.
    work_start: str | None
    work_end: str | None
    hours_per_week: int | None
    languages: list[str]

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(frozen=True)
class Archetype:
    key: str
    weight: int
    years: tuple[int, int]
    # (minimum years, title) in ascending order; the last row whose minimum
    # the candidate meets wins.
    titles: tuple[tuple[int, str], ...]
    # (taxonomy id, probability) - each rolled independently.
    certs: tuple[tuple[str, float], ...]
    # Exactly one system of record, chosen by weight ("" = none).
    erp: tuple[tuple[str, int], ...]
    # (taxonomy id, probability) - each rolled independently.
    tools: tuple[tuple[str, float], ...]
    gaap: tuple[str, ...]
    industries: tuple[str, ...]
    # Certifications outside the taxonomy the model may mention (go to other_certifications).
    international: bool = False
    extra_languages: tuple[tuple[str, float], ...] = field(default_factory=tuple)


ARCHETYPES: tuple[Archetype, ...] = (
    Archetype(
        key="bookkeeper",
        weight=14,
        years=(1, 18),
        titles=((0, "Bookkeeper"), (4, "Full-Charge Bookkeeper"), (9, "Senior Bookkeeper")),
        certs=(("cpb", 0.25), ("cb", 0.15), ("quickbooks_proadvisor", 0.45)),
        erp=(("quickbooks", 80), ("xero", 15), ("sage", 5)),
        tools=(("bill_com", 0.4), ("gusto", 0.35), ("expensify", 0.2), ("excel", 0.9), ("google_sheets", 0.4)),
        gaap=("cash vs accrual basis", "bank and credit card reconciliations", "1099 preparation", "sales tax filings"),
        industries=(
            "professional_services",
            "retail",
            "hospitality",
            "construction",
            "real_estate",
            "nonprofit",
            "legal",
        ),
    ),
    Archetype(
        key="ap_ar",
        weight=10,
        years=(1, 15),
        titles=(
            (0, "Accounts Payable Specialist"),
            (3, "Accounts Receivable Specialist"),
            (6, "AP/AR Supervisor"),
            (10, "Accounts Payable Manager"),
        ),
        certs=(("cb", 0.1),),
        erp=(
            ("sap", 25),
            ("netsuite", 25),
            ("oracle_erp_cloud", 15),
            ("microsoft_dynamics_365", 20),
            ("quickbooks", 15),
        ),
        tools=(("bill_com", 0.35), ("concur", 0.35), ("expensify", 0.15), ("excel", 0.95), ("avalara", 0.1)),
        gaap=("three-way match", "vendor reconciliations", "accruals and cut-off", "cash application", "collections"),
        industries=("logistics", "manufacturing", "healthcare", "retail", "construction", "hospitality"),
    ),
    Archetype(
        key="payroll",
        weight=8,
        years=(2, 20),
        titles=((0, "Payroll Specialist"), (5, "Senior Payroll Specialist"), (9, "Payroll Manager")),
        certs=(("cpp", 0.5), ("fpc", 0.3), ("shrm_cp", 0.1)),
        erp=(("adp", 50), ("paychex", 15), ("paylocity", 15), ("gusto", 10), ("rippling", 5), ("workday", 5)),
        tools=(("excel", 0.9), ("quickbooks", 0.25), ("netsuite", 0.15)),
        gaap=(
            "multi-state payroll tax",
            "payroll journal entries and accruals",
            "benefits reconciliation",
            "year-end W-2 and 1099",
        ),
        industries=("healthcare", "hospitality", "retail", "professional_services", "education", "manufacturing"),
    ),
    Archetype(
        key="staff_accountant",
        weight=14,
        years=(1, 8),
        titles=((0, "Staff Accountant"), (3, "Accountant"), (5, "Senior Accountant")),
        certs=(("cpa", 0.3),),
        erp=(("quickbooks", 50), ("netsuite", 30), ("sage_intacct", 12), ("xero", 4), ("microsoft_dynamics_365", 4)),
        tools=(
            ("excel", 1.0),
            ("blackline", 0.15),
            ("floqast", 0.15),
            ("bill_com", 0.3),
            ("expensify", 0.2),
            ("google_sheets", 0.2),
        ),
        gaap=(
            "US GAAP month-end close",
            "accruals and prepaids",
            "fixed assets and depreciation",
            "ASC 842 lease accounting",
            "balance sheet reconciliations",
        ),
        industries=(
            "saas",
            "technology",
            "healthcare",
            "ecommerce",
            "professional_services",
            "manufacturing",
            "media_and_entertainment",
        ),
    ),
    Archetype(
        key="senior_accountant",
        weight=12,
        years=(5, 15),
        titles=((0, "Senior Accountant"), (8, "Accounting Manager"), (12, "Assistant Controller")),
        certs=(("cpa", 0.55), ("cma", 0.08)),
        erp=(
            ("netsuite", 45),
            ("sap", 15),
            ("oracle_erp_cloud", 10),
            ("microsoft_dynamics_365", 10),
            ("sage_intacct", 10),
            ("quickbooks", 10),
        ),
        tools=(
            ("excel", 1.0),
            ("quickbooks", 0.25),
            ("blackline", 0.3),
            ("floqast", 0.25),
            ("avalara", 0.15),
            ("bill_com", 0.2),
            ("power_bi", 0.15),
        ),
        gaap=(
            "ASC 606 revenue recognition",
            "ASC 842 leases",
            "multi-entity consolidations",
            "intercompany eliminations",
            "audit support and PBC lists",
            "technical accounting memos",
        ),
        industries=(
            "saas",
            "technology",
            "consumer_packaged_goods",
            "healthcare",
            "fintech",
            "manufacturing",
            "ecommerce",
            "real_estate",
        ),
    ),
    Archetype(
        key="controller",
        weight=8,
        years=(10, 25),
        titles=((0, "Controller"), (15, "Corporate Controller"), (20, "VP, Controller")),
        certs=(("cpa", 0.75), ("cma", 0.15), ("cgma", 0.1)),
        erp=(
            ("netsuite", 35),
            ("sap", 15),
            ("oracle_erp_cloud", 10),
            ("microsoft_dynamics_365", 15),
            ("sage_intacct", 15),
            ("workday", 10),
        ),
        tools=(
            ("excel", 1.0),
            ("quickbooks", 0.3),
            ("adaptive_planning", 0.25),
            ("blackline", 0.3),
            ("floqast", 0.2),
            ("avalara", 0.15),
            ("carta", 0.15),
        ),
        gaap=(
            "SOX 404 controls",
            "audit readiness and Big 4 audits",
            "US GAAP technical accounting",
            "IFRS reporting",
            "purchase accounting",
            "ERP implementation",
        ),
        industries=(
            "manufacturing",
            "saas",
            "healthcare",
            "consumer_packaged_goods",
            "logistics",
            "energy",
            "construction",
            "hospitality",
        ),
    ),
    Archetype(
        key="fpa",
        weight=12,
        years=(1, 16),
        titles=(
            (0, "Financial Analyst"),
            (3, "Senior Financial Analyst"),
            (6, "FP&A Manager"),
            (11, "Director of FP&A"),
        ),
        certs=(("cfa", 0.1), ("cma", 0.1), ("cpa", 0.1)),
        erp=(("netsuite", 30), ("sap", 15), ("oracle_erp_cloud", 10), ("workday", 10), ("", 35)),
        tools=(
            ("excel", 1.0),
            ("adaptive_planning", 0.35),
            ("anaplan", 0.2),
            ("planful", 0.1),
            ("vena", 0.08),
            ("power_bi", 0.35),
            ("tableau", 0.2),
            ("looker", 0.15),
            ("sql", 0.3),
            ("python", 0.12),
            ("snowflake", 0.1),
            ("google_sheets", 0.3),
        ),
        gaap=(
            "annual budgeting and rolling forecasts",
            "variance analysis",
            "SaaS metrics (ARR, churn, CAC)",
            "three-statement modelling",
            "board and investor reporting",
            "headcount planning",
        ),
        industries=(
            "saas",
            "technology",
            "ecommerce",
            "fintech",
            "healthcare",
            "consumer_packaged_goods",
            "media_and_entertainment",
            "retail",
        ),
    ),
    Archetype(
        key="tax",
        weight=8,
        years=(2, 20),
        titles=((0, "Tax Associate"), (3, "Tax Senior"), (7, "Tax Manager"), (13, "Senior Tax Manager")),
        certs=(("cpa", 0.6), ("ea", 0.3)),
        erp=(("quickbooks", 60), ("", 40)),
        tools=(("excel", 1.0), ("avalara", 0.1), ("netsuite", 0.1)),
        gaap=(
            "ASC 740 income tax provision",
            "federal and multi-state corporate returns",
            "partnership and S-corp returns",
            "sales and use tax",
            "R&D credit studies",
            "IRS notice resolution",
        ),
        industries=("accounting_services", "accounting_services", "real_estate", "professional_services", "technology"),
    ),
    Archetype(
        key="audit",
        weight=8,
        years=(1, 18),
        titles=(
            (0, "Audit Associate"),
            (3, "Audit Senior"),
            (6, "Internal Auditor"),
            (9, "Senior Internal Auditor"),
            (13, "Audit Manager"),
        ),
        certs=(("cpa", 0.5), ("cia", 0.3), ("cisa", 0.12), ("cfe", 0.12)),
        erp=(("sap", 30), ("oracle_erp_cloud", 20), ("netsuite", 15), ("workday", 10), ("", 25)),
        tools=(("excel", 1.0), ("alteryx", 0.15), ("power_bi", 0.2), ("tableau", 0.1), ("sql", 0.15), ("jira", 0.1)),
        gaap=(
            "financial statement audits",
            "SOX 404 testing",
            "internal controls over financial reporting",
            "risk assessment and audit planning",
            "IT general controls",
            "PCAOB standards",
        ),
        industries=(
            "accounting_services",
            "banking",
            "insurance",
            "healthcare",
            "financial_services",
            "manufacturing",
            "energy",
        ),
    ),
    Archetype(
        key="revenue",
        weight=6,
        years=(3, 14),
        titles=((0, "Revenue Accountant"), (5, "Senior Revenue Accountant"), (9, "Revenue Recognition Manager")),
        certs=(("cpa", 0.5),),
        erp=(("netsuite", 70), ("sap", 15), ("workday", 15)),
        tools=(("excel", 1.0), ("salesforce", 0.6), ("stripe", 0.45), ("blackline", 0.2), ("sql", 0.15)),
        gaap=(
            "ASC 606 five-step model",
            "SSP analysis and contract modifications",
            "deferred revenue and billing",
            "commissions capitalisation (ASC 340-40)",
            "order-to-cash controls",
        ),
        industries=("saas", "fintech", "technology", "media_and_entertainment", "telecommunications"),
    ),
    Archetype(
        key="cost",
        weight=5,
        years=(3, 20),
        titles=((0, "Cost Accountant"), (6, "Senior Cost Accountant"), (11, "Plant Controller")),
        certs=(("cma", 0.4), ("cpa", 0.25)),
        erp=(("sap", 45), ("microsoft_dynamics_365", 30), ("oracle_erp_cloud", 15), ("netsuite", 10)),
        tools=(("excel", 1.0), ("power_bi", 0.3), ("tableau", 0.1), ("sql", 0.15)),
        gaap=(
            "standard costing and variance analysis",
            "inventory valuation (ASC 330)",
            "bill of materials and overhead absorption",
            "cycle counts and physical inventory",
            "capex tracking",
        ),
        industries=(
            "manufacturing",
            "consumer_packaged_goods",
            "food_and_beverage",
            "automotive",
            "pharmaceuticals",
            "agriculture",
        ),
    ),
    Archetype(
        key="treasury",
        weight=3,
        years=(2, 18),
        titles=((0, "Treasury Analyst"), (5, "Senior Treasury Analyst"), (9, "Treasury Manager")),
        certs=(("cfa", 0.15), ("cpa", 0.15)),
        erp=(("sap", 35), ("oracle_erp_cloud", 25), ("netsuite", 25), ("workday", 15)),
        tools=(("excel", 1.0), ("sql", 0.2), ("power_bi", 0.25), ("stripe", 0.1)),
        gaap=(
            "13-week cash forecasting",
            "bank relationship and account management",
            "FX hedging (ASC 815)",
            "debt covenant compliance",
            "investment policy and short-term liquidity",
        ),
        industries=("banking", "energy", "retail", "financial_services", "logistics", "manufacturing"),
    ),
    Archetype(
        key="international",
        weight=6,
        years=(2, 20),
        titles=(
            (0, "Assistant Accountant"),
            (3, "Financial Accountant"),
            (6, "Management Accountant"),
            (10, "Finance Manager"),
            (15, "Financial Controller"),
        ),
        certs=(("acca", 0.5), ("ca", 0.35), ("cgma", 0.1)),
        erp=(("xero", 35), ("sage", 25), ("netsuite", 15), ("sap", 15), ("quickbooks", 10)),
        tools=(("excel", 1.0), ("google_sheets", 0.2), ("power_bi", 0.2), ("bill_com", 0.05)),
        gaap=(
            "IFRS reporting",
            "UK GAAP (FRS 102) statutory accounts",
            "VAT / GST returns",
            "group consolidation with a US parent",
            "local statutory audit",
        ),
        industries=(
            "professional_services",
            "fintech",
            "saas",
            "ecommerce",
            "hospitality",
            "manufacturing",
            "real_estate",
        ),
        international=True,
    ),
    Archetype(
        key="cfo",
        weight=3,
        years=(15, 30),
        titles=((0, "Director of Finance"), (18, "VP Finance"), (22, "Fractional CFO")),
        certs=(("cpa", 0.6), ("cfa", 0.1), ("cma", 0.1)),
        erp=(("netsuite", 55), ("quickbooks", 25), ("sage_intacct", 20)),
        tools=(("excel", 1.0), ("carta", 0.4), ("adaptive_planning", 0.35), ("google_sheets", 0.3), ("bill_com", 0.3)),
        gaap=(
            "Series A-C fundraising and diligence",
            "board reporting and KPI packs",
            "GAAP conversion from cash basis",
            "first-year audit readiness",
            "equity accounting and cap tables",
        ),
        industries=("saas", "technology", "fintech", "ecommerce", "healthcare", "consumer_packaged_goods"),
    ),
    Archetype(
        key="nonprofit_gov",
        weight=4,
        years=(2, 20),
        titles=(
            (0, "Grants Accountant"),
            (4, "Fund Accountant"),
            (8, "Nonprofit Controller"),
            (14, "Director of Finance"),
        ),
        certs=(("cpa", 0.3), ("cgma", 0.05)),
        erp=(("quickbooks", 45), ("sage_intacct", 40), ("microsoft_dynamics_365", 15)),
        tools=(("excel", 1.0), ("bill_com", 0.3), ("expensify", 0.2), ("adp", 0.15)),
        gaap=(
            "ASC 958 net asset reporting",
            "restricted vs unrestricted funds",
            "Uniform Guidance and single audit",
            "GASB reporting",
            "Form 990 preparation",
            "grant budgeting and drawdowns",
        ),
        industries=("nonprofit", "education", "government", "healthcare"),
    ),
)

# ---------------------------------------------------------------------------
# Timezones and locations
# ---------------------------------------------------------------------------

# (IANA zone, weight, region key for names, cities)
US_ZONES: tuple[tuple[str, int, str, tuple[str, ...]], ...] = (
    (
        "America/New_York",
        28,
        "us",
        (
            "New York, NY",
            "Boston, MA",
            "Atlanta, GA",
            "Charlotte, NC",
            "Philadelphia, PA",
            "Miami, FL",
            "Tampa, FL",
            "Raleigh, NC",
            "Pittsburgh, PA",
            "Jersey City, NJ",
        ),
    ),
    (
        "America/Chicago",
        22,
        "us",
        (
            "Chicago, IL",
            "Austin, TX",
            "Dallas, TX",
            "Houston, TX",
            "Minneapolis, MN",
            "Nashville, TN",
            "Kansas City, MO",
            "Milwaukee, WI",
            "San Antonio, TX",
        ),
    ),
    ("America/Denver", 8, "us", ("Denver, CO", "Salt Lake City, UT", "Boise, ID", "Albuquerque, NM", "Boulder, CO")),
    ("America/Phoenix", 3, "us", ("Phoenix, AZ", "Scottsdale, AZ", "Tucson, AZ")),
    (
        "America/Los_Angeles",
        16,
        "us",
        (
            "Los Angeles, CA",
            "San Francisco, CA",
            "Seattle, WA",
            "Portland, OR",
            "San Diego, CA",
            "Oakland, CA",
            "Sacramento, CA",
            "Las Vegas, NV",
        ),
    ),
)

INTERNATIONAL_ZONES: tuple[tuple[str, int, str, tuple[str, ...]], ...] = (
    ("America/Toronto", 20, "ca", ("Toronto, ON", "Ottawa, ON", "Montreal, QC")),
    ("Europe/London", 26, "uk", ("London, UK", "Manchester, UK", "Edinburgh, UK", "Bristol, UK")),
    ("Europe/Dublin", 6, "uk", ("Dublin, Ireland",)),
    ("Europe/Berlin", 6, "de", ("Berlin, Germany", "Munich, Germany")),
    ("Europe/Paris", 4, "fr", ("Paris, France", "Lyon, France")),
    ("Asia/Manila", 14, "ph", ("Manila, Philippines", "Cebu City, Philippines")),
    ("Asia/Kolkata", 12, "in", ("Bengaluru, India", "Pune, India", "Hyderabad, India")),
    ("Australia/Sydney", 6, "au", ("Sydney, Australia", "Melbourne, Australia")),
    ("America/Mexico_City", 3, "mx", ("Mexico City, Mexico", "Guadalajara, Mexico")),
    ("America/Bogota", 3, "co", ("Bogota, Colombia", "Medellin, Colombia")),
)

# ---------------------------------------------------------------------------
# Qualifications by country
# ---------------------------------------------------------------------------
#
# The archetypes roll the US names ("cpa", "ca", "cma"). Nobody in London holds
# a CPA: `_localize` turns each rolled qualification into the one a candidate
# in that time zone would actually hold. It runs after the draw and takes no
# random numbers, so every other fact of every slot is unchanged by it.

US_QUALIFICATIONS = {"cpa": "cpa_us"}

# zone -> {rolled id: local id}. "cpa" left out of a zone stays the ambiguous
# `cpa`: a CPA from a body the equivalence map does not list (the Philippine
# CPA in Manila), which must not pass as a US CPA equivalent.
LOCAL_QUALIFICATIONS: dict[str, dict[str, str]] = {
    "America/Toronto": {"cpa": "cpa_canada", "ca": "cpa_canada"},
    "Europe/Dublin": {"cpa": "ca_ireland", "ca": "ca_ireland", "cma": "cima"},
    "Australia/Sydney": {"cpa": "cpa_australia", "ca": "ca_anz"},
    "Asia/Kolkata": {"cpa": "ca_icai", "ca": "ca_icai"},
    "Asia/Manila": {"ca": "acca"},
}
# Everywhere else abroad (Berlin, Paris, Bogota, ...) the international
# qualification is the ACCA.
ELSEWHERE_QUALIFICATIONS = {"cpa": "acca", "ca": "acca"}

# The UK has three chartered bodies and an Irish neighbour. Chartered
# candidates drawn in London are dealt round this list in slot order, moving
# to Dublin or Edinburgh where the body calls for it, so the seed always has
# an Irish-qualified candidate and a Scottish CA (the zone weights alone draw
# no Dublin slot at 200 candidates).
UK_CHARTERED: tuple[tuple[str, str, str | None], ...] = (
    ("ca_ireland", "Europe/Dublin", "Dublin, Ireland"),
    ("aca_icaew", "Europe/London", None),
    ("ca_icas", "Europe/London", "Edinburgh, UK"),
    ("acca", "Europe/London", None),
)
UK_QUALIFICATIONS = {"cma": "cima"}

# Share of non-international archetypes that still sit outside the US.
ABROAD_SHARE = 0.08

ZONE_LANGUAGE: dict[str, tuple[str, float]] = {
    "America/Mexico_City": ("es", 1.0),
    "America/Bogota": ("es", 1.0),
    "Europe/Paris": ("fr", 1.0),
    "Europe/Berlin": ("de", 1.0),
    "Asia/Manila": ("tl", 1.0),
    "Asia/Kolkata": ("hi", 0.8),
    "America/Toronto": ("fr", 0.3),
}

# Extra languages rolled for everyone (spoken at a professional level).
COMMON_LANGUAGES: tuple[tuple[str, float], ...] = (
    ("es", 0.12),
    ("pt", 0.03),
    ("zh", 0.03),
    ("fr", 0.03),
    ("ko", 0.02),
    ("vi", 0.02),
)

# ---------------------------------------------------------------------------
# Names: fictional, diverse, region-aware so a resume reads coherently.
# ---------------------------------------------------------------------------

NAMES: dict[str, tuple[tuple[str, ...], tuple[str, ...]]] = {
    "us": (
        (
            "Ada",
            "Marcus",
            "Priya",
            "Daniel",
            "Aisha",
            "Kevin",
            "Sofia",
            "Tyler",
            "Mei",
            "Andre",
            "Hannah",
            "Jamal",
            "Lucia",
            "Ethan",
            "Nadia",
            "Brandon",
            "Keiko",
            "Omar",
            "Rachel",
            "Diego",
            "Naomi",
            "Victor",
            "Grace",
            "Samuel",
            "Leila",
            "Connor",
            "Fatima",
            "Miguel",
            "Erin",
            "Darnell",
            "Yuki",
            "Patrick",
            "Ana",
            "Jordan",
            "Simone",
            "Trevor",
            "Ingrid",
            "Malik",
            "Chloe",
            "Hector",
            "Tamara",
            "Wesley",
            "Rosa",
            "Isaac",
            "Bianca",
            "Nathan",
            "Carmen",
            "Derek",
            "Amara",
            "Julian",
            "Whitney",
            "Raj",
            "Monica",
            "Elijah",
            "Renee",
            "Colin",
            "Zainab",
            "Gavin",
            "Delia",
            "Terrence",
        ),
        (
            "Okafor",
            "Nguyen",
            "Patel",
            "Thompson",
            "Rahman",
            "Brooks",
            "Alvarez",
            "Kim",
            "Chen",
            "Washington",
            "Schultz",
            "Carter",
            "Moreno",
            "Fitzgerald",
            "Hassan",
            "Reyes",
            "Tanaka",
            "Abernathy",
            "Goldberg",
            "Ortiz",
            "Lindqvist",
            "Jackson",
            "Petrov",
            "Delgado",
            "Whitaker",
            "Iyer",
            "Sullivan",
            "Castillo",
            "Nakamura",
            "Bryant",
            "Kowalski",
            "Ramirez",
            "Owens",
            "Mensah",
            "Vasquez",
            "Huang",
            "Pierce",
            "Adeyemi",
            "Sandoval",
            "Novak",
            "Blackwell",
            "Torres",
            "Hoffman",
            "Osei",
            "Marchetti",
            "Ferreira",
            "Cho",
            "Dawson",
            "Beaumont",
            "Ellison",
        ),
    ),
    "ca": (
        ("Liam", "Emma", "Noah", "Olivia", "Amélie", "Gabriel", "Aiden", "Zoe", "Arjun", "Maya", "Lucas", "Chantal"),
        ("Tremblay", "MacDonald", "Singh", "Roy", "Gagnon", "Wong", "Campbell", "Bouchard", "Lam", "Fraser"),
    ),
    "uk": (
        (
            "Oliver",
            "Amelia",
            "George",
            "Isla",
            "Harry",
            "Freya",
            "Tariq",
            "Imogen",
            "Callum",
            "Niamh",
            "Rhys",
            "Esme",
            "Kwame",
            "Poppy",
        ),
        (
            "Hughes",
            "Whitfield",
            "Okonkwo",
            "Ahmed",
            "Murray",
            "Bennett",
            "Chowdhury",
            "Fletcher",
            "O'Connor",
            "Walsh",
            "Pritchard",
            "Dunne",
        ),
    ),
    "de": (
        ("Lukas", "Lena", "Jonas", "Hannah", "Felix", "Mira"),
        ("Becker", "Schneider", "Wagner", "Fischer", "Yilmaz", "Krause"),
    ),
    "fr": (
        ("Camille", "Théo", "Inès", "Louis", "Manon", "Adrien"),
        ("Martin", "Lefèvre", "Girard", "Benali", "Rousseau", "Dubois"),
    ),
    "ph": (
        (
            "Maria Cristina",
            "Jose Miguel",
            "Angelica",
            "Paolo",
            "Kristine",
            "Ramon",
            "Joyce Anne",
            "Marco",
            "Bea",
            "Carlo",
        ),
        ("Santos", "Reyes", "Dela Cruz", "Bautista", "Villanueva", "Mendoza", "Garcia", "Aquino", "Navarro", "Ramos"),
    ),
    "in": (
        ("Ananya", "Rohan", "Shreya", "Karthik", "Neha", "Vikram", "Divya", "Aditya", "Pooja", "Siddharth"),
        ("Sharma", "Krishnan", "Mehta", "Reddy", "Banerjee", "Nair", "Joshi", "Kulkarni", "Chatterjee", "Rao"),
    ),
    "au": (
        ("Jack", "Charlotte", "Lachlan", "Mia", "Cooper", "Tahlia"),
        ("Wilson", "Nguyen", "Taylor", "Papadopoulos", "Harris", "Kelly"),
    ),
    "mx": (("Valeria", "Alejandro", "Fernanda", "Emilio"), ("Hernández", "López", "Jiménez", "Salazar")),
    "co": (("Camila", "Santiago", "Valentina", "Andrés"), ("Rodríguez", "Gómez", "Cárdenas", "Restrepo")),
}

SOURCES: tuple[tuple[str, int], ...] = (("upload", 55), ("linkedin", 25), ("referral", 15), ("agency", 5))
ARCHIVED_SHARE = 0.05

AVAILABILITY: tuple[tuple[Availability, int, tuple[int, int] | None], ...] = (
    ("immediate", 25, (0, 3)),
    ("two_weeks", 35, (14, 21)),
    ("one_month", 25, (30, 45)),
    ("unavailable", 5, None),
    ("unknown", 10, None),
)


# Working hours (local), hours a week. A candidate who has said when they can
# start has answered these too; one whose availability is unknown or
# unavailable has answered none, and is excluded from matching.
WORKING_HOURS: tuple[tuple[str, int], ...] = (
    ("09:00-17:00", 40),
    ("08:00-16:00", 15),
    ("08:30-17:00", 10),
    ("10:00-18:00", 15),
    ("07:00-15:00", 8),
    ("12:00-20:00", 7),
    ("13:00-17:00", 5),
)
# Zones a long way from the US, where some candidates keep US hours overnight.
US_HOURS_ABROAD: dict[str, tuple[str, float]] = {
    "Asia/Manila": ("21:00-06:00", 0.75),
    "Asia/Kolkata": ("18:30-03:30", 0.5),
}
HOURS_PER_WEEK: tuple[tuple[str, int], ...] = (("40", 78), ("32", 8), ("30", 5), ("20", 9))
PART_TIME_ARCHETYPES = {"bookkeeper", "cfo"}
PART_TIME_HOURS_PER_WEEK: tuple[tuple[str, int], ...] = (("40", 35), ("30", 15), ("25", 15), ("20", 25), ("15", 10))


def _working_week(seed: int, index: int, zone: str, archetype: str) -> tuple[str, str, int]:
    """(work_start, work_end, hours_per_week) for one slot. Drawn from an RNG
    of its own, so adding these fields did not reshuffle the rest of the plan
    (the stored resumes were written for it)."""
    rng = random.Random(f"{seed}:{index}:working-week")
    hours = _weighted(rng, list(WORKING_HOURS))
    if zone in US_HOURS_ABROAD:
        night, share = US_HOURS_ABROAD[zone]
        if rng.random() < share:
            hours = night
    start, end = hours.split("-")
    weekly = PART_TIME_HOURS_PER_WEEK if archetype in PART_TIME_ARCHETYPES else HOURS_PER_WEEK
    per_week = int(_weighted(rng, list(weekly)))
    if hours == "13:00-17:00":
        per_week = min(per_week, 20)
    return start, end, per_week


def _with_working_week(seed: int, slot: Slot) -> Slot:
    if slot.available_in_days is None:
        return slot
    start, end, per_week = _working_week(seed, slot.index, slot.timezone, slot.archetype)
    return replace(slot, work_start=start, work_end=end, hours_per_week=per_week)


def _weighted(rng: random.Random, options: list[tuple[str, int]]) -> str:
    total = sum(w for _, w in options)
    roll = rng.uniform(0, total)
    acc = 0.0
    for value, weight in options:
        acc += weight
        if roll <= acc:
            return value
    return options[-1][0]


def _title_for(arch: Archetype, years: int) -> str:
    title = arch.titles[0][1]
    for minimum, candidate in arch.titles:
        if years >= minimum:
            title = candidate
    return title


def _ascii_slug(text: str) -> str:
    import unicodedata

    plain = unicodedata.normalize("NFKD", text).encode("ascii", "ignore").decode()
    return "".join(ch for ch in plain.lower() if ch.isalnum())


def _localize(slots: list[Slot]) -> list[Slot]:
    """Swap each rolled qualification for the one held where the candidate lives."""
    us_zones = {z[0] for z in US_ZONES}
    dealt = 0
    out: list[Slot] = []
    for slot in slots:
        zone, location = slot.timezone, slot.location
        if zone in us_zones:
            mapping = US_QUALIFICATIONS
        elif zone == "Europe/London":
            mapping = dict(UK_QUALIFICATIONS)
            if {"cpa", "ca"} & set(slot.certifications):
                local, zone, city = UK_CHARTERED[dealt % len(UK_CHARTERED)]
                dealt += 1
                location = city or location
                mapping |= {"cpa": local, "ca": local}
        else:
            mapping = LOCAL_QUALIFICATIONS.get(zone, ELSEWHERE_QUALIFICATIONS)
        certs: list[str] = []
        for cid in slot.certifications:
            local = mapping.get(cid, cid)
            if local not in certs:
                certs.append(local)
        out.append(replace(slot, certifications=certs, timezone=zone, location=location))
    return out


def build_plan(count: int = DEFAULT_COUNT, seed: int = PLAN_SEED) -> list[Slot]:
    """Return `count` slots. Same (count, seed) -> same plan, every time."""
    rng = random.Random(seed)
    tax = taxonomy.load()
    weighted_archetypes = [(a.key, a.weight) for a in ARCHETYPES]
    by_key = {a.key: a for a in ARCHETYPES}
    used_emails: set[str] = set()
    used_names: set[str] = set()
    slots: list[Slot] = []

    for index in range(1, count + 1):
        arch = by_key[_weighted(rng, weighted_archetypes)]

        # Where they are.
        abroad = arch.international or rng.random() < ABROAD_SHARE
        zones = INTERNATIONAL_ZONES if abroad else US_ZONES
        zone = _weighted(rng, [(z[0], z[1]) for z in zones])
        _, _, region, cities = next(z for z in zones if z[0] == zone)
        location = rng.choice(cities)

        # Who they are.
        firsts, lasts = NAMES[region]
        for _ in range(100):
            full_name = f"{rng.choice(firsts)} {rng.choice(lasts)}"
            if full_name not in used_names:
                break
        else:  # pragma: no cover - pools are large enough
            full_name = f"{rng.choice(firsts)} {rng.choice(lasts)} {index}"
        used_names.add(full_name)
        first, last = full_name.split(" ", 1)
        email = f"{_ascii_slug(first)}.{_ascii_slug(last)}@example.com"
        if email in used_emails:
            email = f"{_ascii_slug(first)}.{_ascii_slug(last)}{index}@example.com"
        used_emails.add(email)
        phone = f"+1 ({rng.randint(201, 989)}) 555-{rng.randint(100, 199):04d}" if not abroad else ""

        # What they have done.
        lo, hi = arch.years
        years = round(rng.triangular(lo, hi, lo + (hi - lo) * 0.25))
        certs = [cid for cid, p in arch.certs if rng.random() < p]
        erp = _weighted(rng, list(arch.erp))
        software = [erp] if erp else []
        for sid, p in arch.tools:
            if sid not in software and rng.random() < p:
                software.append(sid)
        gaap = rng.sample(arch.gaap, k=min(len(arch.gaap), rng.randint(2, 3)))
        industries = rng.sample(sorted(set(arch.industries)), k=rng.randint(1, 2))

        # When they can start.
        availability = _weighted(rng, [(a, w) for a, w, _ in AVAILABILITY])
        window = next(r for a, _, r in AVAILABILITY if a == availability)
        available_in_days = rng.randint(*window) if window else None

        languages = ["en"]
        if zone in ZONE_LANGUAGE:
            lang, p = ZONE_LANGUAGE[zone]
            if rng.random() < p:
                languages.append(lang)
        for lang, p in COMMON_LANGUAGES:
            if lang not in languages and rng.random() < p:
                languages.append(lang)

        for sid in software:
            assert tax.is_canonical("software", sid), sid
        for iid in industries:
            assert tax.is_canonical("industries", iid), iid

        slots.append(
            Slot(
                index=index,
                id=f"{ID_PREFIX}{index:012d}",
                full_name=full_name,
                email=email,
                phone=phone,
                location=location,
                timezone=zone,
                source=_weighted(rng, list(SOURCES)),
                status="archived" if rng.random() < ARCHIVED_SHARE else "active",
                archetype=arch.key,
                title=_title_for(arch, years),
                years_experience=years,
                certifications=certs,
                software=software,
                industries=industries,
                gaap=gaap,
                availability=availability,
                available_in_days=available_in_days,
                work_start=None,
                work_end=None,
                hours_per_week=None,
                languages=languages,
            )
        )
    # Working hours come after _localize, which can move a candidate to another zone.
    slots = [_with_working_week(seed, slot) for slot in _localize(slots)]
    for slot in slots:
        for cid in slot.certifications:
            assert tax.is_canonical("certifications", cid), cid
    return slots


def summarize(slots: list[Slot]) -> str:
    """Human-readable distribution, for eyeballing a plan before spending tokens."""
    from collections import Counter

    lines = [f"{len(slots)} slots"]

    def block(title: str, counter: Counter[str], top: int = 40) -> None:
        lines.append(f"\n{title}")
        for key, n in counter.most_common(top):
            lines.append(f"  {key:<28} {n:>4}  {100 * n / len(slots):5.1f}%")

    block("archetype", Counter(s.archetype for s in slots))
    block("certifications", Counter(c for s in slots for c in s.certifications))
    lines.append(f"  {'(none)':<28} {sum(1 for s in slots if not s.certifications):>4}")
    block("software", Counter(c for s in slots for c in s.software))
    block("industries", Counter(c for s in slots for c in s.industries))
    block("availability", Counter(s.availability for s in slots))
    block("timezone", Counter(s.timezone for s in slots))
    block("status", Counter(s.status for s in slots))
    years = Counter(
        ("0-2" if y <= 2 else "3-5" if y <= 5 else "6-10" if y <= 10 else "11-20" if y <= 20 else "20+")
        for y in (s.years_experience for s in slots)
    )
    block("years", years)
    return "\n".join(lines)
