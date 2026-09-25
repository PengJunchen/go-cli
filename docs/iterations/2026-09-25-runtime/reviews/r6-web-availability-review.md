# R6 Web availability independent root review

Accepted bounded source slice `e7b456cb80a187a1d28efbfabeca4c55ec6dbab8`, plus comment-only `ee376539fe3dab1d6a0954e28bf09b769883f6c5`. Root reviewed the full server source and two-file delta against the approved adapter handoff. Chat and stream now return HTTP 503 before parsing a body or starting SSE; the fake echo/connected response is removed. Existing bearer authentication still runs before these endpoints, and health remains available subject to that same configured authentication.

Without a token, Start accepts only a literal loopback IP parsed from host:port. Empty hosts, wildcards, remote IPs and unverified hostnames are rejected before Listen; the hostname restriction deliberately avoids a DNS check/use race. Configured authentication preserves non-loopback binding. This is not a Web Agent implementation.

Root enumerated seven top-level Web tests, then independently ran `rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/web` on fixed e7: passed in 1.016s. The builder reported 12 test instances including subtests and clean package lint. The follow-up only removes a duplicated doc-comment line; no behavioral retest was required.

No blocker found within availability scope. Existing Start/Stop concurrent lifecycle behavior and the dashboard presentation remain outside this slice and do not receive lifecycle or UI acceptance here. Full authenticated Agent journey remains deferred pending real R1/R2/R4 wiring.
