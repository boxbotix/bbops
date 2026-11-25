## ADDED Requirements

### Requirement: Read-only SKG lexical and neural search HTTP bridge
The `bbops` host runtime SHALL expose `POST /graph-gateway/search` on the `ServiceManager` HTTP front door gated on runtime readiness (`503 Service Unavailable` prior to readiness; `405 Method Not Allowed` on non-`POST`), strictly decoding bounded JSON requests (`<= 64 KiB`, unknown fields rejected) for only the four read-only `semengine` query verbs (`globalSearch`, `localSearch`, `semantic`, and `similar`) and forwarding them over NATS `graph.query.<verb>` using `semengine/graph` request/response contracts with zero reachability to `graph.mutation.>` or MAVLink actuation subjects.

#### Scenario: Lexical globalSearch and localSearch return indexed SKG matches while pre-ready and malformed requests fail closed
- **WHEN** `bbops` ingests UXS vehicle and external contact entities into `ENTITY_STATES` and clients send `POST /graph-gateway/search` before readiness, with non-`POST` methods or unknown verbs/fields, and after readiness with valid `globalSearch` and `localSearch` payloads
- **THEN** pre-ready calls return `503 Service Unavailable`, non-`POST` calls return `405 Method Not Allowed`, unknown verbs or fields return `400 Bad Request` without publishing to NATS, and valid post-ready `globalSearch`/`localSearch` requests return the `graph.QueryResponse` lexical matches, `indexed_revision`, and `damaged_entities` metadata from `graph-query` / `graph-lexical`

#### Scenario: Neural semantic and similar requests surface structured not_enabled refusals when embeddings are disabled
- **WHEN** `bbops` runs with `graph-query.config.embedding_enabled: false` and a client sends a valid `POST /graph-gateway/search` request for `semantic` or `similar`
- **THEN** the bridge forwards the typed request to `graph.query.semantic` or `graph.query.similar` and returns `graph-query`'s structured `not_enabled` refusal deterministically without `500` panics or connection drops
