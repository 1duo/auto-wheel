# Research volatility compatibility sets

`ResearchPiecewiseLinearVolMeshV1` is a separate mathematical research model.
It is not connected to production selection or the historical replay. No
historical Greek, source, or performance qualification follows from its output.

A complete, strictly increasing volatility mesh covers `[0.0001, 5]`. Each
vertex supplies a price and delta on the same mesh. Their finite binary64 values
define exact rational vertices. Construction copies and hashes the declared
curve; it does not verify that vertices came from the American pricer, converged
grids, or causal market inputs.

For an exact cent-denominated bid/ask interval, explicitly expanded by a supplied
price allowance, the utility solves every original affine price segment. An
increasing or decreasing segment yields its closed compatible fraction interval;
a flat compatible segment contributes its whole domain. The union is therefore
the complete price preimage **within this declared surrogate**. Partly
unattainable spreads retain their compatible portions. An empty preimage fails.

Delta extrema are taken on each compatible fragment of each original segment
before merging volatility components. A connected component may cross knots and
contain an interior delta maximum or minimum. Equal mesh membership does not
make delta globally affine across those knots. The delta image is then expanded
by the supplied delta allowance and merged as a union. The overall envelope is
also retained. A flat price never causes an arbitrary point IV to be selected.

All endpoint calculations, intersections and merging use exact rational
arithmetic. Output conversion rounds interval endpoints outward, rejecting
nonrepresentable enclosures. The allowances are disclosed assumptions, not
demonstrated PDE, grid, or interpolation error bounds.

Further work must bind primary and diagnostic FD outputs to every node, evaluate
held-out volatility points, qualify the numerical/model assumptions and all
causal rate, stock, dividend and contract sources, and prove that each entire
qualified delta set determines eligibility and any relevant winner ordering.
Unknown or boundary-straddling information must block a historical decision.
Balanced puts require eligibility proof; Conservative puts and covered calls
also need ranking proof. These controls are not implemented by the mathematical
utility. The existing frozen studies and full coverage predicates remain intact.

The pure `BuildFDMesh` adapter now samples every declared node on a primary and
distinct diagnostic grid. It retains unrounded node values and differences,
copies its inputs and returned evidence, and binds both the primary curve and
deterministic diagnostic report. A failed numerical node is retained as a
failure; no partial curve is returned. The implementation hash is caller
declared and must be independently checked against the actual research build.
Neither the constructor nor sampled grid agreement verifies causal inputs or
an error bound for the PDE.

`autowheel-volmesh-audit` uses three inputs pinned to the prior 42-case synthetic
audit. It measures full-mesh nodes and midpoint holdouts, compares the known
seed sigma/reference delta with the compatible sets, and retains failures.
The first 19-node study failed one low-volatility two-grid check and 17 midpoint
interpolation checks. The original implementation's 73-node, finer-grid run
failed its at-the-money curve and passed 72 midpoint diagnostics for its
dividend-put curve. Its remaining curve was stopped after the updated study
completed. The original source and partial run observations are retained.

A separate implementation concentrates the exact at-the-money grid on
diffusion and carry/contact scales, tightens the thin-layer residual, and uses
faster solver relaxation where coupling is strong. Ten zero-rate prices/deltas
agreed with an independent European closed form, five positive-rate puts agreed
across two grids, and all 42 pinned reference cases passed the unchanged
diagnostic limits. These are finite checks, not global bounds. A 125-node curve
study passed all 125 nodes and 124 midpoint diagnostics on each of the three
inputs in 617.698 measured seconds. Maximum primary midpoint price and delta
errors were $0.00605664 and 0.00087017. Independent changed-code review passed
for the bounded implementation, with the reviewed file hashes verified.
Near-at-the-money inputs still use the original grid; the exact-at-the-money
checks do not establish their accuracy. Original measurements and source
snapshots are retained; no historical qualification is inferred.
