# Codex usage protection

[简体中文](codex-usage-protection.md)

Open **Settings → General → Codex Usage Protection**. These settings apply to all Codex OAuth connections, including existing configurations after an upgrade.

- **Maximum subscription usage** defaults to **98%** and accepts percentages greater than zero and at most 100. Either subscription window reaching the threshold stops new inference on that connection. Ferry can select another eligible connection; otherwise it returns quota unavailable. Windows reset independently: a short-window reset does not release an exhausted weekly window.
- **Allow paid Credits** defaults to **off**. Enabling it bypasses the threshold. After subscription exhaustion, inference continues only when upstream reports available or unlimited Credits. Additional upstream limits, spending controls and actual upstream refusals still apply. This switch cannot choose how the provider charges an individual request.
- **Reset cards** remain separate. Enabling paid Credits does not enable or redeem AutoReset coupons.

Ferry reads subscription usage, reset times and credit details from Codex inference response headers, including streaming and error responses. Missing or invalid fields never mean zero usage. Lower usage from an out-of-order response cannot replace higher usage already observed in the same reset window.

The first call reads upstream usage. Before sending subsequent inference, Ferry reuses evidence for up to 30 seconds; missing, expired or reset-boundary evidence triggers another usage query. Full usage is reconciled at least every five minutes for limits not present in response headers. If current quota cannot be established, inference is refused. Persisted display caches do not authorize the first call after restart.

This is routing protection based on reported usage, not an upstream spending cap. In-flight requests, concurrency, shared-account clients and delayed accounting can exceed the threshold. The default 98% leaves headroom but cannot guarantee zero overage.

Save through authenticated `PATCH /api/settings` and verify with `GET /api/settings`:

```json
{
  "codex_usage_protection": {
    "max_used_percent": 98,
    "allow_credits": false
  }
}
```

Settings persist in the control database. Saving re-evaluates existing quota cooldowns without changing upstream balances.

Codex version changes schedule catalog synchronization. Newly discovered visible models receive same-name logical models automatically; manually maintained routes are preserved. A paused schedule requires **Sync now**. Read-only catalog previews do not publish configuration. See [model synchronization](../provider-model-sync.md) (Chinese).
