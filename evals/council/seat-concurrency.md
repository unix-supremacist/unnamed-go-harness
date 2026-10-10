# Seat concurrency

question: How many panel seats can the ugh council run concurrently against one provider before queueing, and which exact mechanism in the repository enforces that limit? Cite the code.

reference: Four per provider by default. Runtime.Limit returns a buffered channel of capacity routing.provider_concurrency (default 4, config.go), and every agent stream acquires that per-provider semaphore in streamOnce (agent.go) before calling Provider.Stream. Ten council seats therefore run at most four concurrent streams per provider; excess seats queue on the semaphore.
