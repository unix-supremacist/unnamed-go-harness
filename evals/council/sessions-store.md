# Sessions store

question: For ugh JSONL sessions, append-only per-message files vs SQLite for a personal single-user harness: which should the session store use? Compare simplicity, resume, and crash handling.

angles: evidence-map, alternatives

reference: Keep the append-only per-message JSONL files. For a personal single-user harness JSONL is the simpler store: appending one message is a single append with no schema, migrations, driver or binary format; resume is a sequential read of the file; crash handling degrades to a torn final line that is detected and dropped on load. SQLite's real advantages — transactional multi-row atomicity, concurrent writers, and queryable history — fit workloads this harness does not have (one process, one user, replay is a scan), while adding a dependency plus its own failure modes (corruption, locks, vacuum, migration). Choose SQLite only if requirements grow to concurrent writers or indexed queries over history.
