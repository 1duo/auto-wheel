//! Operator-only factual recovery for a stopped TradePulse journal.
//! Calls only broker observations. Never reviews, submits, or cancels an order.
use anyhow::{Context, Result, bail};
use chrono::{DateTime, Utc};
use clap::Parser;
use fs2::FileExt;
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    fs,
    fs::OpenOptions,
    path::{Path, PathBuf},
};
use tradepulse::pilot::{
    BrokerObservationV1, BrokerOrderStateV1, DirectRobinhoodBroker, LifecycleStateV1, McpEvidence,
    OPERATOR_PAUSE_NEW_RISK_FILE, PilotConfigV1, PilotEventKindV1, PilotLifecycleV1, PilotStore,
    SEALED_CROSS_HOST_HANDOFF_FILE, StopProtectionMode, build_reconciliation_diff,
    load_fresh_oauth_credential, oauth_http_client,
};

#[derive(Parser)]
struct Args {
    #[arg(long)]
    state_dir: PathBuf,
    /// SHA-256 of the broker-assigned order ID, in SYMBOL:HASH form.
    #[arg(long, required = true)]
    order_hash: Vec<String>,
    #[arg(long)]
    audit_evidence: PathBuf,
    #[arg(long)]
    audit_sha256: String,
    /// Append native recovery events. Refuses while a daemon owns its lock.
    #[arg(long)]
    apply_journal: bool,
}
struct Case {
    before: PilotLifecycleV1,
    order: BrokerOrderStateV1,
    rejected_blocks: Vec<String>,
}
fn hash(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}
fn micros(value: f64) -> Result<i64> {
    let scaled = value * 1_000_000.0;
    if !value.is_finite()
        || value < 0.0
        || scaled > 9_000_000_000_000_000.0
        || (scaled - scaled.round()).abs() > 0.000_001
    {
        bail!("quantity is not a bounded exact six-decimal quantity");
    }
    Ok(scaled.round() as i64)
}
fn order_hashes(values: &[String]) -> Result<BTreeMap<String, String>> {
    let mut out = BTreeMap::new();
    for value in values {
        let (symbol, digest) = value.split_once(':').context("expected SYMBOL:HASH")?;
        if symbol.is_empty()
            || !symbol.bytes().all(|c| c.is_ascii_uppercase())
            || digest.len() != 64
            || !digest
                .bytes()
                .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
            || out.insert(symbol.to_owned(), digest.to_owned()).is_some()
        {
            bail!("duplicate or malformed exact order selection");
        }
    }
    Ok(out)
}
fn prepare(
    store: &PilotStore,
    observation: &BrokerObservationV1,
    selected: &BTreeMap<String, String>,
) -> Result<Vec<Case>> {
    observation.validate()?;
    let local = store.load_local_state()?;
    if local.expected_account_identity_hash.as_deref()
        != Some(observation.account.account_identity_hash.as_str())
        || !local.ambiguous_symbols.is_empty()
        || !observation.positions.is_empty()
        || observation.orders.iter().any(|o| !o.terminal)
    {
        bail!("account binding, ambiguity, positions, or working orders prevent recovery");
    }
    let intents = store.order_intents()?;
    let mut cases = Vec::new();
    for lifecycle in store.load_lifecycles()? {
        lifecycle.validate()?;
        if lifecycle.state == LifecycleStateV1::Flat {
            continue;
        }
        if lifecycle.state != LifecycleStateV1::ExitPending
            || micros(lifecycle.current_quantity)? <= 0
        {
            bail!("only a fully proved existing exit_pending lifecycle can be recovered");
        }
        let digest = selected
            .get(&lifecycle.symbol)
            .context("missing exact external exit order")?;
        let matches = observation
            .orders
            .iter()
            .filter(|o| hash(o.broker_order_id.as_bytes()) == *digest)
            .collect::<Vec<_>>();
        if matches.len() != 1 {
            bail!("selected broker exit is missing or duplicated");
        }
        let order = matches[0];
        if order.symbol != lifecycle.symbol
            || order.side != "sell"
            || order.status != "filled"
            || !order.terminal
            || micros(order.quantity)? != micros(lifecycle.current_quantity)?
            || micros(order.filled_quantity)? != micros(order.quantity)?
            || intents
                .iter()
                .any(|i| i.broker_order_id.as_deref() == Some(order.broker_order_id.as_str()))
        {
            bail!("external full-exit identity, quantity, or attribution does not agree");
        }
        let mut blocks = Vec::new();
        for intent in intents.iter().filter(|i| i.symbol == lifecycle.symbol) {
            let summary = local
                .order_intents
                .iter()
                .find(|i| i.intent_key == intent.intent_key)
                .context("intent summary missing")?;
            if summary.terminal {
                continue;
            }
            let p = &intent.payload;
            // A ManagementBlockV1 is a rejected management observation, not a
            // financial order request. No send-capable request is terminalized.
            let key = match (p["exit_intent_key"].as_str(), p["reason"].as_str()) {
                (Some(exit), Some(reason)) if !exit.is_empty() && !reason.is_empty() => {
                    format!("{exit}:blocked:{reason}")
                }
                _ => bail!("unknown nonterminal intent requires independent recovery"),
            };
            if intent.status != "recorded"
                || intent.ambiguous
                || intent.broker_order_id.is_some()
                || p["schema_version"] != "ManagementBlockV1"
                || key != intent.intent_key
                || p.as_object().map(|p| p.len()) != Some(3)
            {
                bail!("nonterminal send-capable or malformed intent prevents retirement");
            }
            blocks.push(intent.intent_key.clone());
        }
        cases.push(Case {
            before: lifecycle,
            order: order.clone(),
            rejected_blocks: blocks,
        });
    }
    if cases.len() != selected.len() || cases.is_empty() {
        bail!("order selection does not exactly cover all non-flat lifecycles");
    }
    // A nonterminal intent belonging to a flat or unknown symbol also blocks.
    for intent in &local.order_intents {
        if !intent.terminal
            && !cases
                .iter()
                .any(|c| c.rejected_blocks.contains(&intent.intent_key))
        {
            bail!("unexplained nonterminal intent outside the recovered lifecycles");
        }
    }
    Ok(cases)
}
fn validate_audit(
    bytes: &[u8],
    expected_hash: &str,
    account_hash: &str,
    cases: &[Case],
    now: DateTime<Utc>,
) -> Result<DateTime<Utc>> {
    if hash(bytes) != expected_hash {
        bail!("audit evidence hash differs");
    }
    let d: Value = serde_json::from_slice(bytes)?;
    let at = DateTime::parse_from_rfc3339(d["at"].as_str().context("audit lacks timestamp")?)?
        .with_timezone(&Utc);
    if now.signed_duration_since(at).num_seconds() > 60 || at > now {
        bail!("fresh account audit required (maximum 60 seconds)");
    }
    let reads = d["reads"].as_array().context("audit reads missing")?;
    if reads
        .iter()
        .any(|r| !r["tool"].as_str().unwrap_or("").starts_with("get_"))
    {
        bail!("audit contains a non-read operation");
    }
    let data = |r: &Value| r["result"].get("data").unwrap_or(&r["result"]).clone();
    let mut account = None;
    for r in reads.iter().filter(|r| r["tool"] == "get_accounts") {
        for a in data(r)["accounts"].as_array().context("accounts missing")? {
            if let Some(number) = a["account_number"].as_str() {
                if hash(number.as_bytes()) == account_hash {
                    if account.replace(number.to_owned()).is_some() {
                        bail!("duplicate bound account");
                    }
                }
            }
        }
    }
    let account = account.context("audit account does not match the native account")?;
    for (tool, collection) in [
        ("get_equity_positions", "positions"),
        ("get_option_positions", "positions"),
        ("get_equity_orders", "orders"),
        ("get_option_orders", "orders"),
    ] {
        let pages = reads
            .iter()
            .filter(|r| r["tool"] == tool)
            .collect::<Vec<_>>();
        if pages.is_empty() {
            bail!("required account audit read missing");
        }
        for r in pages {
            if r["arguments"]["account_number"] != account {
                bail!("audit read is not bound to the native account");
            }
            let v = data(r);
            let rows = v[collection]
                .as_array()
                .context("audit collection missing")?;
            if collection == "positions" && !rows.is_empty() {
                bail!("account positions remain");
            }
            if collection == "orders" {
                for order in rows {
                    let state = order["state"].as_str().context("order state missing")?;
                    if !matches!(
                        state,
                        "filled" | "cancelled" | "canceled" | "rejected" | "failed" | "expired"
                    ) {
                        bail!("visible working or unknown-state account order remains");
                    }
                }
            }
        }
    }
    for c in cases {
        let rows = reads
            .iter()
            .filter(|r| r["tool"] == "get_equity_orders")
            .flat_map(|r| data(r)["orders"].as_array().cloned().unwrap_or_default())
            .collect::<Vec<_>>();
        let rows = rows
            .iter()
            .filter(|o| o["id"] == c.order.broker_order_id)
            .collect::<Vec<_>>();
        if rows.len() != 1 {
            bail!("exact external order missing from fresh independent audit");
        }
        let o = rows[0];
        if o["state"] != "filled" || o["symbol"] != c.before.symbol || o["side"] != "sell" {
            bail!("independent audit contradicts selected external fill");
        }
        // Preserve the authoritative decimal executions in the journal proof.
        let parse = |v: &Value| -> Result<i64> {
            let s = v.as_str().context("broker decimal must be a string")?;
            let (whole, frac) = s.split_once('.').unwrap_or((s, ""));
            if whole.is_empty()
                || !whole.bytes().all(|c| c.is_ascii_digit())
                || frac.len() > 6
                || !frac.bytes().all(|c| c.is_ascii_digit())
            {
                bail!("malformed broker quantity");
            }
            let mut digits = frac.to_owned();
            while digits.len() < 6 {
                digits.push('0');
            }
            whole
                .parse::<i64>()?
                .checked_mul(1_000_000)
                .and_then(|x| x.checked_add(digits.parse::<i64>().ok()?))
                .context("broker quantity overflow")
        };
        let executions = o["executions"]
            .as_array()
            .context("individual executions missing")?;
        let mut total = 0i64;
        let latest_entry = c
            .before
            .fills
            .iter()
            .map(|f| DateTime::parse_from_rfc3339(&f.filled_at))
            .collect::<std::result::Result<Vec<_>, _>>()?
            .into_iter()
            .max()
            .context("entry fill timestamp missing")?;
        for e in executions {
            let t = DateTime::parse_from_rfc3339(
                e["timestamp"]
                    .as_str()
                    .context("execution timestamp missing")?,
            )?;
            if t < latest_entry || t.with_timezone(&Utc) > at {
                bail!("exit execution is outside the lifecycle or audit");
            }
            total = total
                .checked_add(parse(&e["quantity"])?)
                .context("execution quantity overflow")?;
        }
        if total != micros(c.before.current_quantity)?
            || parse(&o["quantity"])? != total
            || parse(&o["cumulative_quantity"])? != total
        {
            bail!("independent execution quantities disagree");
        }
    }
    Ok(at)
}
fn apply(
    store: &mut PilotStore,
    observation: &BrokerObservationV1,
    evidence: &[McpEvidence],
    cases: &[Case],
    release: &str,
    audit: &[u8],
    at: DateTime<Utc>,
    actor: &str,
) -> Result<()> {
    for e in evidence {
        store.store_mcp_evidence(e)?;
    }
    let audit_record = store.store_evidence("application/json", at, audit)?;
    let mut hashes = observation.evidence_hashes.clone();
    hashes.push(audit_record.evidence_hash);
    store.append_event(Utc::now(),PilotEventKindV1::Observation,hashes.clone(),json!({
        "schema_version":"TradePulseExternalFullExitRetirementV1","operator_binary_sha256":actor,
        "broker_observation":observation,"external_exits":cases.iter().map(|c|json!({
            "before_lifecycle":c.before,"broker_order":c.order,
            "rejected_management_block_intents":c.rejected_blocks
        })).collect::<Vec<_>>()
    }))?;
    for c in cases {
        for key in &c.rejected_blocks {
            store.update_order_intent(key, "rejected", None, false, Utc::now())?;
        }
        let mut closed = c.before.clone();
        closed.set_remaining_quantity(0.0)?;
        closed.protection = StopProtectionMode::UnprotectedPending;
        store.record_lifecycle_with_evidence(&closed, release, &hashes)?;
    }
    let local = store.load_local_state()?;
    if local
        .lifecycles
        .iter()
        .any(|l| !l.flat || l.expected_quantity_micros != 0)
        || local.order_intents.iter().any(|i| !i.terminal)
        || !local.ambiguous_symbols.is_empty()
    {
        bail!("native state is not exactly retired after recovery");
    }
    let diff = build_reconciliation_diff(&local, observation)?;
    if !diff.account_reconciled {
        bail!("post-recovery native broker reconciliation failed");
    }
    store.record_reconciliation(observation, evidence, &diff)?;
    let _ = store.status_snapshot()?; // Independently revalidate materialized native commitments.
    Ok(())
}
fn run() -> Result<()> {
    let args = Args::parse();
    if !args.state_dir.is_absolute() {
        bail!("absolute native state directory required");
    }
    let pause = args.state_dir.join(OPERATOR_PAUSE_NEW_RISK_FILE);
    if !fs::symlink_metadata(&pause)?.file_type().is_file()
        || args.state_dir.join(SEALED_CROSS_HOST_HANDOFF_FILE).exists()
    {
        bail!("native new-risk pause required; an already sealed state cannot be edited");
    }
    let config: PilotConfigV1 =
        serde_json::from_slice(&fs::read(args.state_dir.join("config.json"))?)?;
    if Path::new(&config.state_dir) != args.state_dir
        || config.expected_account_identity_hash.is_none()
    {
        bail!("native config binding missing");
    }
    let _lock = if args.apply_journal {
        let path = args.state_dir.join("daemon.lock");
        if !fs::symlink_metadata(&path)?.file_type().is_file() {
            bail!("plain daemon lock required");
        }
        let lock = OpenOptions::new().read(true).write(true).open(path)?;
        lock.try_lock_exclusive()
            .context("daemon still owns the state; no journal mutation attempted")?;
        Some(lock)
    } else {
        None
    };
    let selected = order_hashes(&args.order_hash)?;
    let mut store = PilotStore::open_read_only(args.state_dir.join("pilot.sqlite3"))
        .context("retirement_stage: native_readonly_store")?;
    let http = oauth_http_client().context("retirement_stage: native_oauth_client")?;
    let credential = load_fresh_oauth_credential(&http)
        .context("retirement_stage: native_oauth_load")?
        .context("retirement_stage: native_oauth_missing")?;
    let (mut broker, _) = DirectRobinhoodBroker::connect(&credential)
        .context("retirement_stage: native_broker_connect")?;
    let (observation, evidence) = broker
        .observe()
        .context("retirement_stage: native_broker_observe")?;
    if config.expected_account_identity_hash.as_deref()
        != Some(observation.account.account_identity_hash.as_str())
    {
        bail!("broker differs from native config");
    }
    let cases = prepare(&store, &observation, &selected)
        .context("retirement_stage: native_recovery_prepare")?;
    let audit = fs::read(&args.audit_evidence)?;
    let at = validate_audit(
        &audit,
        &args.audit_sha256,
        &observation.account.account_identity_hash,
        &cases,
        Utc::now(),
    )
    .context("retirement_stage: independent_audit_validate")?;
    let before_anchor = store.event_anchor_hash()?;
    if args.apply_journal {
        drop(store);
        store = PilotStore::open(args.state_dir.join("pilot.sqlite3"))?;
        if store.event_anchor_hash()? != before_anchor {
            bail!("native event anchor changed before mutation");
        }
        let actor = hash(&fs::read(std::env::current_exe()?)?);
        apply(
            &mut store,
            &observation,
            &evidence,
            &cases,
            &config.release_hash,
            &audit,
            at,
            &actor,
        )?;
    }
    println!(
        "{}",
        json!({
            "schema_version":"TradePulseRetirementRecoveryReportV1","journal_applied":args.apply_journal,
            "financial_mutations_attempted":0,"readiness_for_live_autowheel":false,
            "advanced_oco_coverage_qualified":false,"before_event_anchor":before_anchor,
            "after_event_anchor":store.event_anchor_hash()?,
            "cases":cases.iter().map(|c|json!({"symbol":c.before.symbol,
                "closed_quantity_micros":micros(c.before.current_quantity).unwrap(),
                "broker_order_id_sha256":hash(c.order.broker_order_id.as_bytes()),
                "rejected_management_blocks":c.rejected_blocks.len()})).collect::<Vec<_>>()
        })
    );
    Ok(())
}
fn main() {
    if let Err(error) = run() {
        let stage = error
            .chain()
            .map(|e| e.to_string())
            .find(|e| e.starts_with("retirement_stage:"))
            .unwrap_or_else(|| "retirement_failed_unclassified".to_owned());
        eprintln!("{stage}");
        std::process::exit(1);
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use tradepulse::pilot::{BrokerAccountStateV1, BrokerWatchlistStateV1};
    fn fixture() -> (
        tempfile::TempDir,
        PilotStore,
        BrokerObservationV1,
        BTreeMap<String, String>,
    ) {
        let dir = tempfile::tempdir().unwrap();
        let mut store = PilotStore::open(dir.path().join("pilot.sqlite3")).unwrap();
        let account = hash(b"test-account");
        store.bind_account_identity(&account).unwrap();
        let mut l = PilotLifecycleV1::new(
            "issuer".into(),
            "AAPL".into(),
            Utc::now().date_naive(),
            80_000.0,
            7.747945,
            "entry",
        )
        .unwrap();
        l.record_cumulative_tranche_fill(
            1,
            7.747945,
            100.0,
            (Utc::now() - chrono::Duration::days(1)).to_rfc3339(),
            2.0,
        )
        .unwrap();
        l.state = LifecycleStateV1::ExitPending;
        l.exit_after_close = true;
        l.protection = StopProtectionMode::Software;
        store.record_lifecycle(&l, &"a".repeat(64)).unwrap();
        store.record_order_intent("exit:blocked:position_mismatch","AAPL",&json!({"schema_version":"ManagementBlockV1","exit_intent_key":"exit","reason":"position_mismatch"}),Utc::now()).unwrap();
        let obs = BrokerObservationV1 {
            schema_version: "BrokerObservationV1".into(),
            observed_at: Utc::now().to_rfc3339(),
            account: BrokerAccountStateV1 {
                account_identity_hash: account,
                agentic_account: true,
                cash_only: true,
                margin_enabled: false,
                cash_available: 80_000.0,
                equity: 80_000.0,
            },
            approval_watchlist: BrokerWatchlistStateV1 {
                watchlist_id: Some("watch".into()),
                name: tradepulse::pilot::APPROVAL_WATCHLIST_NAME.into(),
                items: BTreeMap::new(),
            },
            positions: vec![],
            orders: vec![BrokerOrderStateV1 {
                broker_order_id: "external-sell".into(),
                symbol: "AAPL".into(),
                side: "sell".into(),
                quantity: 7.747945,
                filled_quantity: 7.747945,
                status: "filled".into(),
                client_order_id: None,
                terminal: true,
                average_fill_price: Some(110.0),
                stop_price: None,
            }],
            evidence_hashes: vec![],
            observation_hash: String::new(),
        }
        .seal()
        .unwrap();
        let selected = BTreeMap::from([("AAPL".into(), hash(b"external-sell"))]);
        (dir, store, obs, selected)
    }
    #[test]
    fn retirement_requires_exact_full_exit_and_bound_account() {
        let (_d, s, obs, selected) = fixture();
        assert!(prepare(&s, &obs, &selected).is_ok());
        let mut bad = obs.clone();
        bad.orders[0].filled_quantity = 7.0;
        bad = bad.seal().unwrap();
        assert!(prepare(&s, &bad, &selected).is_err());
        let mut bad = obs.clone();
        bad.account.account_identity_hash = "b".repeat(64);
        bad = bad.seal().unwrap();
        assert!(prepare(&s, &bad, &selected).is_err());
        let wrong = BTreeMap::from([("AAPL".into(), hash(b"unrelated"))]);
        assert!(prepare(&s, &obs, &wrong).is_err());
    }
    #[test]
    fn retirement_refuses_send_capable_or_unknown_intents() {
        let (_d, mut s, obs, selected) = fixture();
        s.record_order_intent(
            "unknown",
            "AAPL",
            &json!({"schema_version":"Unknown"}),
            Utc::now(),
        )
        .unwrap();
        assert!(prepare(&s, &obs, &selected).is_err());
        s.update_order_intent("unknown", "reviewed", None, false, Utc::now())
            .unwrap();
        assert!(prepare(&s, &obs, &selected).is_err());
    }
    #[test]
    fn independent_audit_rejects_staleness_wrong_account_and_partial_executions() {
        let (_d, s, obs, selected) = fixture();
        let cases = prepare(&s, &obs, &selected).unwrap();
        let now = Utc::now();
        let mut reads = vec![
            json!({"tool":"get_accounts","arguments":{},"result":{"data":{"accounts":[{"account_number":"test-account"}]}}}),
        ];
        for tool in [
            "get_equity_positions",
            "get_option_positions",
            "get_option_orders",
        ] {
            let key = if tool.ends_with("positions") {
                "positions"
            } else {
                "orders"
            };
            reads.push(json!({"tool":tool,"arguments":{"account_number":"test-account"},"result":{"data":{key:[]}}}));
        }
        reads.push(json!({"tool":"get_equity_orders","arguments":{"account_number":"test-account"},"result":{"data":{"orders":[{
            "id":"external-sell","symbol":"AAPL","side":"sell","state":"filled",
            "quantity":"7.747945","cumulative_quantity":"7.747945",
            "executions":[{"quantity":"7.747945","timestamp":now.to_rfc3339()}]
        }]}}}));
        let audit = json!({"at":now.to_rfc3339(),"reads":reads});
        let check = |v: &Value| {
            let raw = serde_json::to_vec(v).unwrap();
            validate_audit(
                &raw,
                &hash(&raw),
                &obs.account.account_identity_hash,
                &cases,
                now,
            )
        };
        assert!(check(&audit).is_ok());
        let mut bad = audit.clone();
        bad["at"] = json!((now - chrono::Duration::seconds(61)).to_rfc3339());
        assert!(check(&bad).is_err());
        let mut bad = audit.clone();
        bad["reads"][1]["arguments"]["account_number"] = json!("other-account");
        assert!(check(&bad).is_err());
        let mut bad = audit.clone();
        bad["reads"][4]["result"]["data"]["orders"][0]["executions"][0]["quantity"] =
            json!("7.000000");
        assert!(check(&bad).is_err());
    }
    #[test]
    fn native_recovery_preserves_entry_fills_and_proves_flat_materialization() {
        let (_d, mut s, mut obs, selected) = fixture();
        let raw = b"native-read-fixture".to_vec();
        let e = McpEvidence {
            schema_version: "McpEvidenceV1".into(),
            observed_at: Utc::now().to_rfc3339(),
            method: "get_equity_orders".into(),
            http_status: 200,
            content_type: "application/json".into(),
            session_id_present: true,
            response_hash: hash(&raw),
            raw_response: raw,
        };
        obs.evidence_hashes = vec![e.response_hash.clone()];
        obs = obs.seal().unwrap();
        let before = s.load_lifecycles().unwrap()[0].clone();
        let count = s.event_count().unwrap();
        let plan = prepare(&s, &obs, &selected).unwrap();
        apply(
            &mut s,
            &obs,
            &[e],
            &plan,
            &"a".repeat(64),
            b"independent-audit-fixture",
            Utc::now(),
            &"c".repeat(64),
        )
        .unwrap();
        let after = s.load_lifecycles().unwrap()[0].clone();
        assert_eq!(after.fills, before.fills);
        assert_eq!(after.lifecycle_id, before.lifecycle_id);
        assert_eq!(after.state, LifecycleStateV1::Flat);
        assert_eq!(after.current_quantity, 0.0);
        assert!(
            s.load_local_state()
                .unwrap()
                .order_intents
                .iter()
                .all(|i| i.terminal)
        );
        assert!(s.event_count().unwrap() > count);
        s.status_snapshot().unwrap();
    }
}
