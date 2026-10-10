# Aggregator default

question: Should the ugh council's aggregator model default stay mimo-v2.6-pro rather than gpt-sol, given the model cards, tiers and provider routes in the repository? Justify from the cards and routes.

reference: Yes. mimo-v2.6-pro is the high-tier card with good tool calling on static-key routes (opencode-go first, then openrouter), while gpt-sol's card reserves it for cases where its agentic reliability advantage is genuinely needed and warns against routine selection. A council aggregator runs many tool-using turns per question, so the card with verified cheap static-key routes (mimo-v2.6-pro) is the right default.
