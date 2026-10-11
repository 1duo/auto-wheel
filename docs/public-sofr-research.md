# Public SOFR candidate for the historical option model

The official New York Fed API supplied 128 actual daily SOFR observations for
April 1–October 2, 2026. They support a candidate numerical rate for all 126
AutoWheel study sessions, including every one of the 9,324 scheduled decisions.
This component is offline and remains unqualified for historical Greeks. It
does not add observed replay decisions or produce strategy returns.

Run `python3 scripts/audit-public-sofr.py --output NEW_FILE` from the checkout.
The command verifies fixed source and baseline-spec hashes, rejects overwrites,
and retains the actual observations and session selections in the output.
Source receipts and audit (local evidence omitted from this source repository)
record the requests, hashes, and rejected HTTP200 error-page redirect. Public
requests used no Databento credential or data budget.

The candidate uses an explicit flat future-rate assumption. An observed SOFR
percentage is a simple overnight ACT/360 rate. A one-calendar-day accrual maps
to continuous ACT/365 through `365*log1p(percent/100/360)`. Repeating this converted
rate across an option's remaining tenor assumes a flat one-day-compounded future
rate. It is neither an observed forward/OIS curve nor the actual historical
multi-day SOFR holiday accrual. The calculation agrees with a separate
45-digit Decimal logarithm calculation within 1e-15 for every retained rate.

Availability is deliberately modeled after the entire expected publication
day has ended in New York. The algorithm does not invent a precise capture time
from the official approximate 8 a.m. publication schedule. This also waits
through the ordinary same-day revision window. For example, April 2's SOFR was
published April 6 and becomes eligible in this candidate only at April 7,
00:00 New York time. April 6 decisions therefore use April 1's observation.

The bounded SOFR calendar contains weekends, SIFMA full closures, and the
separately announced April 3 exception. SIFMA recommended only an early close
on that day; applying SIFMA alone would incorrectly publish April 2's rate on
April 3. The New York Fed explicitly announced that publication would instead
occur April 6. July 3's separate announcement confirms that closure as well.
The rate series exactly matches the 128 expected effective dates. Revised rows,
missing dates, duplicate dates, unexpected series or fields, and boolean rates
are rejected. Five boundary checks and six mutated-source checks are retained.

The pinned methodology and policy-index snapshots were retrieved October 6.
Their statements support this conservative calendar construction, but a current
historical endpoint and current policy page are not independent proof of every
past publication or vintage. Independent historical-publication validation,
future-rate-model acceptance, and causal integration into an independently
validated American IV/delta model remain required. All nine full-study source
gates remain pending; production and the baseline strategy are unchanged.

Primary sources:

- [Official SOFR API](https://markets.newyorkfed.org/api/rates/secured/sofr/search.json?startDate=2026-04-01&endDate=2026-10-02)
- [New York Fed methodology, publication, and revisions](https://www.newyorkfed.org/markets/reference-rates/additional-information-about-reference-rates)
- [SIFMA holiday recommendations](https://www.sifma.org/resources/general/holiday-schedule)
- [March 12 announcement about April 3](https://www.newyorkfed.org/markets/opolicy/operating_policy_260312a)
- [June 18 announcement about July 3](https://www.newyorkfed.org/markets/opolicy/operating_policy_260618a)
- [Reference-rate policy index](https://www.newyorkfed.org/markets/reference-rates/statements-and-operating-policies)
