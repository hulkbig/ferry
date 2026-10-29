# Provider subscription usage

The Provider list, workspace and Codex editor render the same cached OAuth usage snapshot.
On entry they show cached data immediately, then request a fresh snapshot through the shared,
account-scoped request cache. An animated border and disabled refresh button indicate the request
is pending. A failed refresh retains the last snapshot and exposes the error. Replacing the bound
account invalidates the cached identity.

## Layout

Each Provider occupies a responsive card. Identity, routing status and primary actions form the
header; endpoint and credential details wrap below it. Model counts expand into the complete
upstream and logical model lists. Reset Cooldowns and Delete remain available in More.
A full-width subscription section sits below these details, with readable plan facts,
quota meters and the snapshot timestamp. Model-specific quotas expand separately. The details
and editor use the same quota renderer and keep expanded quota groups during refresh.

Reset credits use compact rows with the credit name, remaining validity, expiration and action.
The available count appears once beside the reset-credit heading. IDs, internal scope,
grant dates and upstream descriptions stay in the confirmation dialog; consumption is explained
there before submission. Provider pages use monochrome surfaces, fine dividers and unfilled
usage sections. Normal usage uses a neutral value and meter; high usage and imminent expiry
retain warning colors. Missing or partial credit details remain explicit.

## Display contract

| Returned data | Display |
| --- | --- |
| plan_type | Pro, Plus, Free, Go, Business (including team), Enterprise or Edu; preserve unknown plan names |
| primary_window / secondary_window | Render only present windows; determine label from limit_window_seconds, never from position or plan name |
| 604800 seconds | Weekly limit |
| 18000 seconds | 5-hour limit |
| Other positive duration | Actual day/hour/minute/second duration |
| Missing duration or percentage | Generic usage window / Not reported; never invent 0% or unlimited usage |
| additional_rate_limits | Separate named model groups with both returned windows |
| credits | Reported balance or explicit unlimited **credits**, without implying unlimited subscription usage |
| rate_limit_reset_credits.available_count | Available usage resets, including explicit zero; omit when absent |
| spend_control.individual_limit | Reported seat spending limit, used and remaining values; no currency inference |
| Limit or spending reached | Explicit status independent of numeric percentage |
| Reset time | Absolute reset_at, or observed_at + reset_after_seconds; never a stale countdown |

Window labels come from their reported durations, not the account plan or the position
of a field. A weekly primary window with no secondary window must remain weekly;
missing data does not establish an unlimited quota. Different model groups may
report different windows.

OpenAI's Codex pricing documentation describes varying account limits and separate Spark limits:
https://developers.openai.com/codex/pricing
The account snapshot is authoritative for the current window duration and reset time.

## Verification

subscription-usage.test.mjs covers weekly-only Pro, dual-window Plus, independent Spark windows,
organizational credits and budgets, unknown plans, missing fields and HTML escaping.
quota-cache.test.mjs covers refresh deduplication, retained stale snapshots, navigation and
account replacement. Both are included in scripts/test.sh and the console's npm test command.
