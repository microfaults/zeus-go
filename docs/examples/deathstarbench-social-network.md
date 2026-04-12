# Death Star Bench — Social Network Workflow Example

This document demonstrates what a realistic workflow looks like in the Zeus DSL v2. The target is DeathStarBench's Social Network benchmark: a microservice application fronted by an NGINX gateway that exposes Thrift RPC endpoints over HTTP.

Online-boutique is a 4-step browse with a single user and 9 products. It validates the engine but does not exercise the DSL. This example is deeper: multi-step dependent sessions, parallel fan-out, weighted body variants, cross-step data extraction, and realistic think-time placement.

## Target benchmark

DeathStarBench (ASPLOS 2019, Cornell/Berkeley) is an open-source microservice benchmark suite. The Social Network sub-benchmark is the most widely cited and has the richest dependency graph. Services communicate via Thrift RPC; an NGINX container fronts the public HTTP gateway. The canonical wrk2 Lua scripts drive traffic against the HTTP endpoints.

Reference repository: `delimitrou/DeathStarBench` on GitHub. Reference endpoints are documented in `socialNetwork/wrk2/scripts/*.lua` and the Thrift IDL at `socialNetwork/social_network.thrift`.

## Services hit

| Service | Role | Endpoint(s) |
|---|---|---|
| `nginx-thrift` | HTTP gateway, routes to Thrift backends | All `/wrk2-api/*` |
| `user-service` | User registration, login, auth | `/wrk2-api/user/register`, `/wrk2-api/user/login` |
| `social-graph-service` | Follow/unfollow relationships | `/wrk2-api/user/follow`, `/wrk2-api/user/unfollow` |
| `compose-post-service` | Post creation, text parsing, media, mentions, fan-out | `/wrk2-api/post/compose` |
| `home-timeline-service` | Read aggregated timeline from followed users | `/wrk2-api/home-timeline/read` |
| `user-timeline-service` | Read a single user's post history | `/wrk2-api/user-timeline/read` |
| `post-storage-service` | Post persistence (MongoDB) | Internal, called by compose and timeline services |
| `media-service` | Media attachment handling | Internal |
| `text-service` | Text parsing (mentions, URLs) | Internal |
| `url-shorten-service` | URL shortening for embedded links | Internal |

The workflow only hits `nginx-thrift` — the internal service calls are invisible to the load driver but visible in OTel traces, which is exactly where atropos observes them.

## Canonical user session

A typical social-network user session in this benchmark:

1. **Login** (required). POST credentials, extract auth token.
2. **Read home timeline** (required). GET the aggregated feed. This is the heaviest read path: it fans out through social-graph → post-storage → cache.
3. **Maybe read a specific user's timeline** (60%). A secondary read that exercises user-timeline-service independently.
4. **Follow two users in parallel** (required, fanned out). Two concurrent POST requests to social-graph-service. Demonstrates `parallel` with `wait: all`.
5. **Pause 1.5–3 seconds.** Realistic think-time before composing.
6. **Compose a post** (required, with four weighted body variants). The core write path: compose fans out to text-service, media-service, user-service (mention validation), post-storage, and home-timeline (fan-out to followers). Body variants model the realistic mix of short text posts, media posts, replies, and DMs.
7. **Maybe like the post and optionally reshare** (40%, nested 50%). An optional engagement sequence that extracts the composed post's ID and feeds it back.

## Workflow JSON in DSL v2

```json
{
  "$schema": "https://faults-lab.dev/zeus/workflow.schema.json",
  "version": "2",
  "name": "dsb-socialnet-compose-session",
  "description": "Login, read timelines, parallel follows, compose post with variants, optional like/reshare",
  "targets": [
    "nginx-thrift", "user-service", "home-timeline-service",
    "user-timeline-service", "social-graph-service",
    "compose-post-service", "post-storage-service",
    "url-shorten-service", "media-service"
  ],
  "base_url": "http://nginx-thrift:8080",
  "estimated_rps_per_vu": 2,
  "default_delay": { "min_ms": 400, "max_ms": 1200 },
  "thresholds": {
    "http_req_failed":   ["rate<0.05"],
    "http_req_duration": ["p(95)<3000"]
  },

  "data_schema": {
    "pools": {
      "users": {
        "fields": ["id", "username", "region", "follow_ids"],
        "min_size": 200
      },
      "posts": {
        "fields": ["body_template"],
        "min_size": 50
      },
      "reply_targets": {
        "fields": ["post_id", "author_username"],
        "min_size": 20,
        "optional": true
      }
    }
  },

  "root": {
    "type": "sequence",
    "id": "session",
    "children": [

      {
        "type": "request",
        "id": "login",
        "method": "POST",
        "path": "/wrk2-api/user/login",
        "headers": { "Content-Type": "application/json" },
        "body": {
          "username": "{{data.users.username}}",
          "password": "password"
        },
        "expect": { "status": [200] },
        "extract": {
          "auth_token": "jsonpath:$.token",
          "user_id":    "jsonpath:$.user_id"
        },
        "after_delay": { "min_ms": 100, "max_ms": 300 }
      },

      {
        "type": "request",
        "id": "read-home-timeline",
        "method": "GET",
        "path": "/wrk2-api/home-timeline/read?user_id={{steps.login.user_id}}&start=0&stop=10",
        "headers": { "Authorization": "Bearer {{steps.login.auth_token}}" },
        "expect": { "status": [200] }
      },

      {
        "type": "optional",
        "probability": 0.6,
        "child": {
          "type": "request",
          "id": "read-user-timeline",
          "method": "GET",
          "path": "/wrk2-api/user-timeline/read?user_id={{steps.login.user_id}}&start=0&stop=10",
          "headers": { "Authorization": "Bearer {{steps.login.auth_token}}" },
          "expect": { "status": [200] }
        }
      },

      {
        "type": "parallel",
        "id": "fan-out-follows",
        "wait": "all",
        "children": [
          {
            "type": "request",
            "id": "follow-a",
            "method": "POST",
            "path": "/wrk2-api/user/follow",
            "headers": {
              "Content-Type":  "application/json",
              "Authorization": "Bearer {{steps.login.auth_token}}"
            },
            "body": {
              "user_id":     "{{steps.login.user_id}}",
              "followee_id": "{{data.users.follow_ids.0}}"
            },
            "expect": { "status": [200, 204] }
          },
          {
            "type": "request",
            "id": "follow-b",
            "method": "POST",
            "path": "/wrk2-api/user/follow",
            "headers": {
              "Content-Type":  "application/json",
              "Authorization": "Bearer {{steps.login.auth_token}}"
            },
            "body": {
              "user_id":     "{{steps.login.user_id}}",
              "followee_id": "{{data.users.follow_ids.1}}"
            },
            "expect": { "status": [200, 204] }
          }
        ]
      },

      { "type": "delay", "min_ms": 1500, "max_ms": 3000 },

      {
        "type": "request",
        "id": "compose-post",
        "method": "POST",
        "path": "/wrk2-api/post/compose",
        "headers": {
          "Content-Type":  "application/json",
          "Authorization": "Bearer {{steps.login.auth_token}}"
        },
        "body": {
          "username":    "{{data.users.username}}",
          "user_id":     "{{steps.login.user_id}}",
          "text":        "{{data.posts.body_template}}",
          "media_ids":   [],
          "media_types": [],
          "post_type":   0
        },
        "variants": [
          {
            "weight": 60,
            "set": {
              "body.text":      "{{data.posts.body_template}} #fromRegion_{{data.users.region}}",
              "body.post_type": 0
            }
          },
          {
            "weight": 20,
            "set": {
              "body.text":        "{{data.posts.body_template}} [with media]",
              "body.media_ids":   [1],
              "body.media_types": ["photo"],
              "body.post_type":   0
            }
          },
          {
            "weight": 15,
            "set": {
              "body.text":      "@{{data.reply_targets.author_username}} re: {{data.reply_targets.post_id}}",
              "body.post_type": 2
            }
          },
          {
            "weight": 5,
            "set": {
              "body.text":      "DM: {{data.posts.body_template}}",
              "body.post_type": 3
            }
          }
        ],
        "expect": { "status": [200, 201] },
        "extract": {
          "post_id": "jsonpath:$.post_id"
        },
        "after_delay": { "min_ms": 2000, "max_ms": 4000 }
      },

      {
        "type": "optional",
        "probability": 0.4,
        "child": {
          "type": "sequence",
          "id": "engage",
          "children": [
            {
              "type": "request",
              "id": "like-post",
              "method": "POST",
              "path": "/wrk2-api/post/like",
              "headers": {
                "Content-Type":  "application/json",
                "Authorization": "Bearer {{steps.login.auth_token}}"
              },
              "body": {
                "user_id": "{{steps.login.user_id}}",
                "post_id": "{{steps.compose-post.post_id}}"
              },
              "expect": { "status": [200, 204] }
            },
            {
              "type": "optional",
              "probability": 0.5,
              "child": {
                "type": "request",
                "id": "reshare-post",
                "method": "POST",
                "path": "/wrk2-api/post/compose",
                "headers": {
                  "Content-Type":  "application/json",
                  "Authorization": "Bearer {{steps.login.auth_token}}"
                },
                "body": {
                  "username":      "{{data.users.username}}",
                  "user_id":       "{{steps.login.user_id}}",
                  "text":          "RT: {{steps.compose-post.post_id}}",
                  "media_ids":     [],
                  "media_types":   [],
                  "post_type":     1
                },
                "expect": { "status": [200, 201] }
              }
            }
          ]
        }
      }

    ]
  }
}
```

## Data schema

The workflow declares three data pools. The first two are required; the third is optional.

### `users` pool (required, min 200 rows)

Each row represents a pre-registered user in the social network.

```json
{
  "id": "12345",
  "username": "alice_smith",
  "region": "us-west",
  "follow_ids": ["67890", "11111"]
}
```

`follow_ids` is an array of user IDs that this user will follow during the session. The workflow reads `follow_ids.0` and `follow_ids.1` as the targets for the parallel follow fan-out.

### `posts` pool (required, min 50 rows)

Templates for post body text.

```json
{
  "body_template": "Just had a great meeting about distributed systems. #microservices"
}
```

Variant 0 (60%) appends a region hashtag. Variant 1 (20%) attaches a media ID. Variant 2 (15%) turns the body into a reply mentioning another user. Variant 3 (5%) turns it into a DM.

### `reply_targets` pool (optional, min 20 rows)

Needed only by variant 2 (reply). If absent, variant 2 degrades: `{{data.reply_targets.author_username}}` resolves to empty string, and the reply looks like `@  re:` — ugly but non-fatal.

```json
{
  "post_id": "98765",
  "author_username": "bob_jones"
}
```

## Where manteion sources this data

The data pool sources tie back to the three-signal strategy in `VISION.md`:

- **`users` pool:** Projected from cache-box dumps of past `POST /wrk2-api/user/register` responses. Each register response body contains the `user_id` and `username`. Manteion reads the cache-box export, groups entries by the register endpoint, parses the stored bodies, deduplicates by `user_id`, and enriches with `region` from trace attributes (or synthesizes it). The `follow_ids` array is populated from the social-graph-service's cache-box entries for the `/follow` endpoint (extracting `followee_id` per `user_id`).

- **`posts` pool:** Projected from cache-box dumps of past `POST /wrk2-api/post/compose` requests. Manteion reads the `text` field from each cached compose request body and uses it as a `body_template`. Deduplication is optional since post texts naturally vary.

- **`reply_targets` pool:** Projected from cache-box dumps of compose requests where `post_type == 2` (reply). Each reply's original `text` mentions a `@username` and references a `post_id`.

Cache-box gives the body content. OTel traces give the endpoint ordering (which manteion used to reconstruct this workflow's tree shape). Prometheus gives the target RPS and VU count.

## Dependency and timing notes

- The **parallel follow fan-out** (`fan-out-follows`) runs both follow requests concurrently inside a single VU iteration. In k6 terms, this uses `http.asyncRequest` for both children and `Promise.all` to await them. The VU does not advance to the `delay` node until both follows complete. This is how a real user's browser would fan out follow XHR calls.

- **Delays are chosen by the author.** The explicit `delay` node between the parallel block and `compose-post` models a human pausing to think before writing. The per-request `after_delay` on `login` (100-300ms) models the browser redirect after auth. The `after_delay` on `compose-post` (2-4s) models the user reading their post after publishing. Everywhere else, the `default_delay` of 400-1200ms kicks in.

- **Extract visibility across the tree.** `login` produces `auth_token` and `user_id`, which every subsequent step reads via `{{steps.login.auth_token}}` and `{{steps.login.user_id}}`. The parallel children (`follow-a`, `follow-b`) read `auth_token` from the outer session scope via the upward walk. `compose-post` extracts `post_id`, which the nested `engage` sequence reads. The `engage` sequence's `like-post` and `reshare-post` are inside an `optional`, but because `optional` is scope-transparent, they can read any extract from the enclosing session scope.

- **Variant mix.** 60% of compose requests are plain text posts, 20% include a media attachment, 15% are replies mentioning another user, 5% are DMs. This is expressed as a single `compose-post` step with a `variants` list. The engine picks one variant per iteration, applies its `set` patches to the base body, then resolves templates. Manteion can verify the observed mix by reading `zeus_variant_picks_total` from Prometheus.

## What this example does not model

These are deliberate scope exclusions. The DSL can express all of them; the example omits them for clarity.

- **User registration flow.** In a real experiment, a setup workflow (run once before the main load) registers 200+ users. This example assumes users already exist.
- **Media upload.** Variant 1 passes `media_ids: [1]` as if media were pre-uploaded. A full model would have a prior step `POST /wrk2-api/media/upload` that returns the media ID.
- **Search and recommendations.** `GET /wrk2-api/user/search` and `GET /wrk2-api/recommendation` are valid read paths but not included.
- **Multi-session state persistence.** This workflow is stateless across VU iterations. A user who followed Alice in iteration 1 will try to follow her again in iteration 2. The social-graph-service handles the idempotency, but the load profile does not model "I only follow new users."
- **Unfollow.** A realistic session might unfollow before following to keep the graph stable under long runs.
- **Read-after-write on the compose.** After composing a post, a real user might read their own user timeline to see it appear. Adding that step is straightforward — it would be a sibling inside the `engage` optional sequence.
