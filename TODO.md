# Technical Debt & Future Improvements

## Response Body Double-Parsing Pattern

**File:** main.go (Backoff line 291, CheckRetry line 340)

**Current Behavior:**
- Both Backoff and CheckRetry call `unmarshalResp()` on the same HTTP response
- `unmarshalResp()` (helper.go:205) reassigns `resp.Body` with `io.NopCloser` to make it readable multiple times
- Pattern is documented in helper.go line 204 comment

**Why It Works:**
- HTTP response bodies are single-read streams
- `unmarshalResp()` explicitly reassigns body after reading to enable multiple reads
- Has been working reliably in production

**Why It's Technical Debt:**
- Fragile coupling: breaks if `unmarshalResp()` implementation changes
- Implicit dependency: not obvious from function signatures
- Double parsing: minor performance overhead

**Better Solution:**
- Parse response body once in CheckRetry (called first by retryablehttp library)
- Store parsed `GitHubError` in request context
- Retrieve from context in Backoff (called second)
- Explicit data flow, no body reassignment magic

**Why Not Fixed Now:**
- Works reliably
- More complex refactor (context passing, ~30 lines changed)
- Other issues (#2-4) are actual bugs with production impact

**Decision:** Document for future consideration, prioritize fixing actual bugs.

**Estimated Effort:** 2-3 hours (implementation + testing)
