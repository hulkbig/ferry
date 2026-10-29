# Change models. Keep your integration.

[中文](logical-models.md) · [Hermes example](hermes.en.md)

Your application calls a logical model such as `work`. Ferry's routes determine the actual provider, model and account. When the underlying model changes, the client keeps the same endpoint, key and model name.

## Configure around the task

1. Add the accounts or APIs in **Providers**, using accurate upstream model IDs and checking their protocols and capabilities.
2. Create a logical model in **Models & Routing**, then arrange its connections and primary/fallback steps. Use a name that fits the workload rather than a particular vendor.
3. Create an inference key that permits this logical model. Configure the client with Ferry's base URL, that key and `work`. These values do not need to change when an upstream model is replaced.
4. Change connections and routes in Ferry when needed. Validate and simulate before applying, then check new requests, tool calls and results on the new route.

`work` is an example, not a built-in model. A stable name does not make upstream capabilities or outputs identical. Check the tools, streaming, protocols and context behavior your workload needs; use `exact_only` where appropriate.

## Hermes is one example

Hermes can call `work` while existing subscriptions or APIs sit behind it. Changing the actual model or account does not require reconfiguring Hermes. Other clients supporting the relevant protocol can use the same approach. See the [Hermes guide](hermes.en.md).

The diagram shows model A completing one request, followed by a route change and model B handling the next. It does not depict replacing or splicing a stream already producing visible output. Recovery conditions and budgets are covered in the [long-task guide](long-tasks.md), currently in Chinese.
