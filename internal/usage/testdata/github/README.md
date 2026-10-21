Recorded from the three GitHub organization billing endpoints using `gh api`
on mini, 2026-10-21 04:43 EDT, for October 2026. Raw responses and commands are
retained outside this public repository at
`/home/franz/artifacts/hs-pi-dashboard/issue-45/fixtures`.

Public fixtures replace the organization with `example-org`, replace budget
IDs, remove alert recipients, shorten summary rows, and replace summary usage
with round synthetic quantities. Field names, value types, the zero-valued
Copilot AI-credit row, and all four $0 ProductPricing stop budgets preserve the
actual transport schema. Tests also cover the documented plural
`budget_product_skus`, pagination, mutated bad responses and an empty month.

Primary contracts checked 2026-10-21:
https://docs.github.com/en/rest/billing/usage
https://docs.github.com/en/rest/billing/budgets

`summary-subscription.json` preserves a `copilot_for_business` row newly
observed on the same three-API live qualification at 05:42 EDT. The org is
replaced with `example-org` and monetary/quantity values with synthetic $0.50
and one unit. The raw capture remains in the artifact directory above.
The earlier statement that summary totals excluded subscription SKUs was
incorrect; every summary row contributes to product totals.
