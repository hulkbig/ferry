# AutoReset: redeem credits before they expire

[中文](autoreset.md) · [Connect Hermes](hermes.en.md)

When a Codex account has an available reset credit, Ferry can redeem it before expiry so you do not have to watch the deadline. It uses existing credits; it does not create new ones. The upstream decides whether any usage windows are eligible for reset.

## Settings

Open **Settings → Automatic Codex Reset Cards** in the console:

| Setting | Default | Meaning |
| --- | --- | --- |
| Automatic redemption | Enabled | Can be disabled |
| Remaining validity threshold | 20 minutes | Attempt redemption when a credit enters this expiry window; accepts 1–1440 minutes |

Ferry checks on startup and every 30 seconds afterwards for enabled Codex connections. Connections sharing an upstream account are deduplicated. Each scan selects at most one credit per account, choosing the earliest-expiring eligible credit. Network and upstream responses affect timing; completion exactly 20 minutes before expiry is not guaranteed.

## When it skips a credit

Automatic redemption skips unavailable, expired, unknown-expiry or incompatible credits. It also waits when the account has an unresolved reset operation requiring review. Disabling automation, the provider or the connection prevents new automatic redemptions. Standby validation instances do not redeem credits.

Redemption consumes the selected credit and affects all clients sharing that upstream account. The trigger is expiry time, not a local usage-percentage threshold.

## Check the outcome

The subscription usage section in **Providers** shows reset credits and their history, distinguishing manual requests from automatic triggers with timestamps and upstream results. “No eligible window to reset” is not a successful reset. An uncertain submission needs review; the automatic task does not blindly repeat a submitted request.

The homepage illustrates one redemption scenario. It does not imply every account has credits or that every redemption resets a window. This guide was checked against the current scheduler, redemption implementation and regression cases. This documentation change did not operate a live account or consume a credit.
