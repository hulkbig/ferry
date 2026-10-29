# Use a ChatGPT subscription in Hermes

[中文](hermes.md) · [Docker installation](docker.en.md)

Connect your existing accounts to Ferry and give Hermes a set of routes. When an account is rate limited, Ferry can wait or switch according to your policy. You do not have to keep switching accounts by hand.

## Prepare routes in Ferry

1. Add your accounts or APIs in **Providers**. ChatGPT accounts use Codex OAuth; other subscriptions must use a login method supported by Ferry. Save and enable the relevant Provider.
2. Create a logical model, such as `work`, in **Models & Routing**. Arrange eligible accounts and upstream models into primary and fallback steps. `work` is an example, not a built-in model.
3. Set concurrency, waiting and recovery conditions. Validate and simulate the route before applying it. A shared model name does not prove that every account supports tools or follow-up turns.
4. Create an **inference API key** scoped to that model for Hermes. Do not give the client upstream account credentials or a management token.

Within a route step, Ferry considers protocol/capability compatibility, known quota pressure, recent throttling, health and concurrency. Known exhausted quota windows that have not reset affect eligibility; unknown quota is not treated as unlimited. Moving between route steps still depends on your routing and recovery policy.

## Select Ferry in Hermes

Run:

```sh
hermes model
```

Follow your Hermes version's prompts for a custom OpenAI-compatible endpoint:

| Setting | Value |
| --- | --- |
| Base URL | Same-host Docker example: `http://localhost:17810/v1`; use your configured HTTPS inference endpoint remotely |
| API key | The Ferry inference key you created |
| Model | `work`, or your actual logical model name |

Hermes documents this custom-endpoint setup in its [official provider guide](https://hermes-agent.nousresearch.com/docs/integrations/providers). If Hermes and Ferry run in separate containers, `localhost` refers to the current container; use a reachable service address instead.

## Verify an actual task

Start with a simple conversation, then run a task that includes tool calls and tool-result follow-ups. In Ferry's **Requests**, inspect the selected connection, recovery reasons and final result. Check records in **Usage & Billing**.

This setup has been checked against Hermes documentation and Ferry's routing implementation. It is not a claim that every Hermes version, subscription or model combination has been tested live. Route simulation does not call an upstream and cannot replace live task verification.

## How quota is used

Ferry does not merge different plans into one balance or expand provider entitlements. It uses the connected, eligible routes to reduce manual account switching. Quota information depends on what each provider reports; permissions, reset windows and usage limits still apply.

Recovery has time and attempt budgets and stops on client cancellation. Ferry does not splice another model stream after visible output is committed. A fallback API may charge per call; check costs and access conditions before adding it. See the [long-task guide](long-tasks.md) (Chinese) for further configuration.
