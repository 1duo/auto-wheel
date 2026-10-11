# Contract selection modes

Implemented on Linux msi and in the installed AutoWheel menubar. Current preset:
**Balanced**. Trading remains paused because the shared coordinator's ownership
import and execution qualification are incomplete.

| Mode | Eligible put absolute delta | Contract and symbol priority |
| --- | --- | --- |
| Conservative | 0.05–0.15 | Lowest absolute delta, then highest net premium per collateral dollar per calendar day |
| Balanced | 0.15–0.35 | Highest net premium per collateral dollar per calendar day |

Both use the nearest eligible weekly expiry, tomorrow through eight calendar
days. Failure at that expiry skips the symbol. Fresh quotes, standard contracts,
positive net credit, liquidity, cash and ownership rules remain required. Delta
is a risk proxy; conservative selection cannot guarantee avoiding assignment.

Balanced is interpreted as selling a cash-secured put → assignment → covered
call. No stop-loss threshold was supplied or added. Confirmed assigned shares
in either mode retain the aggressive covered-call exit: earliest eligible daily
or weekly expiry, configured delta 0.50–0.85, highest delta first, then time-value
yield. Below-cost calls remain allowed. Calls receive allocation priority.

Choose **Selection** in the menubar. A change pauses new submissions under the
broker's submission lock, removes activation, saves a private preset and
restarts AutoWheel. It verifies a fresh response with the requested mode loaded,
read-only execution and HALT present. It does not enable live trading. Pending
changes hide obsolete readiness/plans and reject activation, resume and scans.
Accepted orders continue through reconciliation; inventory is not sold or
appropriated by changing mode.

Before the initial shared ownership import, the observation coordinator has no
registration to revoke. A change may proceed only after durable local pause and
activation removal plus fresh, account/release/policy-bound confirmation of
generation zero, epoch zero and no trading permissions. Registered permissions
otherwise require acknowledged withdrawal.

Verification: 18 targeted Go checks passed with the Linux race detector; Swift
type-check and installed signature/API checks passed. Both actual msi preset
transitions, pending-control rejection and restart loading passed while paused.
Independent control review passed. The installed menu and compact scrolling
layout were visually inspected. TradePulse configuration, supervisor PID,
client binding and coordinator policy were unchanged. No order was submitted.

See deployment evidence (local evidence omitted from this source repository),
control review (local evidence omitted from this source repository), and
[operator instructions](operations.md).

Historical studies remain blocked by missing qualified data and American
Greeks. This production conservative preset differs from the earlier research
variant, which has a zero delta floor and ranks premium yield under a 0.15 cap.
Frozen study inputs are preserved. Five targeted Linux compatibility checks
passed, and both blocked studies were reproduced with the new wheel source
fingerprint. Historical source qualification still precedes mode comparisons;
no performance result is claimed for either preset.
