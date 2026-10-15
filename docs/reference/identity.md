# Identity

A sandbox can prove which sandbox it is to anything that accepts OpenID Connect (OIDC) tokens
(AWS STS, Google Cloud workload identity federation, Vault, a service of your own) without holding
any credential. loam-api is the issuer: a process in the sandbox asks for a token for an
audience, and whoever receives it checks loam's signature against loam's public keys.

| Method and path | |
|---|---|
| `GET /.well-known/openid-configuration` | OIDC discovery; no key needed |
| `GET /.well-known/jwks.json` | the public keys; no key needed |
| `GET /identity/token?audience=<aud>&ttl_s=600` | a token; **from inside the sandbox only**, on its [API socket](inside.md) |

## Getting a token

Inside the sandbox, on `/run/loam/api.sock`:

```console
$ curl -sS --unix-socket /run/loam/api.sock 'http://loam/identity/token?audience=sts.amazonaws.com&ttl_s=900'
{"token":"eyJhbGciOiJSUzI1NiIsImtpZCI6ImpXY3ZFWHlK...","expires_at":"2026-10-12T09:36:26Z"}
```

`audience` is required: the `aud` the receiver expects (`sts.amazonaws.com` for AWS, your
service's URL for your own). `ttl_s` is whole seconds from 1 to 3600, default 600.

```console
$ curl -sS --unix-socket /run/loam/api.sock 'http://loam/identity/token'
{"error":{"code":"invalid","message":"audience is required"}}
$ curl -sS --unix-socket /run/loam/api.sock 'http://loam/identity/token?audience=x&ttl_s=5000'
{"error":{"code":"invalid","message":"ttl_s must be a number of seconds from 1 to 3600"}}
```

From TypeScript inside the sandbox: `await LoamClient.inside().identityToken('sts.amazonaws.com')`.

A project key can't get a token: there is no such route under `/v1`. Only code running in the
sandbox can, which is the point.

## What's in it

An RS256 JWT. Its header and claims, decoded:

```json
{"alg":"RS256","kid":"jWcvEXyJW-uNKXi-otD4oD3pvV8s_3TpoXs9tpKzq1g","typ":"JWT"}
```

```json
{
  "iss": "https://api.loam.example.com",
  "sub": "project:docs:sandbox:sb-2kjiso7vff5b",
  "aud": "sts.amazonaws.com",
  "iat": 1791796886, "nbf": 1791796886, "exp": 1791797786,
  "jti": "a6cd035797068b260bd2e70e113d5ad5",
  "project": "docs",
  "sandbox": "sb-2kjiso7vff5b",
  "sandbox_name": "scratch",
  "template": "base",
  "labels": {}
}
```

| Claim | |
|---|---|
| `iss` | the API's base URL, `https://api.loam.example.com` |
| `sub` | `project:<project>:sandbox:<id>`: stable for the sandbox's life, and never reused |
| `aud` | the audience asked for |
| `iat`, `nbf`, `exp` | issued, valid from, expires (Unix seconds) |
| `jti` | a unique ID per token |
| `project`, `sandbox`, `sandbox_name`, `template`, `labels` | the sandbox as it is when the token is made |

`kid` in the header is the RFC 7638 thumbprint of the signing key. Trust `sub` and `project` for
authorization; a name or a label is something anyone with the project's key can change.

## Discovery and keys

```console
$ curl -sS https://api.loam.example.com/.well-known/openid-configuration
{"claims_supported":["iss","sub","aud","iat","nbf","exp","jti","project","sandbox","sandbox_name","template","labels"],
 "id_token_signing_alg_values_supported":["RS256"],"issuer":"https://api.loam.example.com",
 "jwks_uri":"https://api.loam.example.com/.well-known/jwks.json","response_types_supported":["id_token"],
 "subject_types_supported":["public"]}
$ curl -sS https://api.loam.example.com/.well-known/jwks.json
{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"jWcvEXyJW-uNKXi-otD4oD3pvV8s_3TpoXs9tpKzq1g","n":"ppJYzNCcvQhcN6zU...","e":"AQAB"}]}
```

When loam rotates its key, the old one stays in the JWKS while tokens it signed can still be live,
so a verifier that fetches the JWKS by `kid` (and refetches on an unknown `kid`) keeps working.

A deployment without a signing key answers `501 unsupported` on these routes and on
`/identity/token`.

## Verifying a token

Any OIDC or JWT library does this given the issuer URL. By hand, in Node 20 or later with no
packages:

```js
// verify.mjs: check a loam identity token against loam's JWKS.
import { createPublicKey, verify } from 'node:crypto'

const issuer = 'https://api.loam.example.com'
const audience = 'sts.amazonaws.com'
const token = process.argv[2]

const [h, p, sig] = token.split('.')
const header = JSON.parse(Buffer.from(h, 'base64url'))
const claims = JSON.parse(Buffer.from(p, 'base64url'))

const conf = await (await fetch(`${issuer}/.well-known/openid-configuration`)).json()
const { keys } = await (await fetch(conf.jwks_uri)).json()
const jwk = keys.find((k) => k.kid === header.kid)
if (header.alg !== 'RS256' || !jwk) throw new Error('unknown key or algorithm')

const ok = verify('RSA-SHA256', Buffer.from(`${h}.${p}`), createPublicKey({ key: jwk, format: 'jwk' }), Buffer.from(sig, 'base64url'))
const now = Date.now() / 1000
if (!ok) throw new Error('bad signature')
if (claims.iss !== conf.issuer || claims.aud !== audience) throw new Error('wrong issuer or audience')
if (now < claims.nbf || now >= claims.exp) throw new Error('expired or not yet valid')
console.log('valid:', claims.sub, claims.sandbox_name, claims.labels)
```

```console
$ TOKEN=$(loam run scratch -- curl -sS --unix-socket /run/loam/api.sock \
    'http://loam/identity/token?audience=sts.amazonaws.com' </dev/null | jq -r .token)
$ node verify.mjs "$TOKEN"
valid: project:docs:sandbox:sb-2kjiso7vff5b scratch { team: 'docs' }
```

## AWS: assume a role with the token

A sketch; it needs an AWS account, so it wasn't run for these docs. Once, in AWS, register loam as
an identity provider and make a role that trusts it for your project:

```sh
aws iam create-open-id-connect-provider \
  --url https://api.loam.example.com \
  --client-id-list sts.amazonaws.com
```

The role's trust policy (`ACCOUNT` is your account ID; the `sub` pattern admits every sandbox in
project `docs`; name one sandbox's ID to admit only it):

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Federated": "arn:aws:iam::ACCOUNT:oidc-provider/api.loam.example.com"},
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {"api.loam.example.com:aud": "sts.amazonaws.com"},
      "StringLike": {"api.loam.example.com:sub": "project:docs:sandbox:*"}
    }
  }]
}
```

Then, in the sandbox, the AWS SDKs and CLI do the exchange themselves when given a file with the
token and the role:

```sh
curl -sS --unix-socket /run/loam/api.sock 'http://loam/identity/token?audience=sts.amazonaws.com' \
  | jq -r .token > /tmp/aws-token
export AWS_WEB_IDENTITY_TOKEN_FILE=/tmp/aws-token
export AWS_ROLE_ARN=arn:aws:iam::ACCOUNT:role/loam-docs
export AWS_ROLE_SESSION_NAME=$(hostname)
aws sts get-caller-identity
```

or explicitly, `aws sts assume-role-with-web-identity --role-arn ... --role-session-name ...
--web-identity-token "$(cat /tmp/aws-token)"`. The token lives 10 minutes by default; the SDKs
read the file again when they need new credentials, so refresh it (a [service](services.md) that
rewrites it every few minutes) for long-running work. The sandbox needs to reach
`sts.amazonaws.com` (and the AWS services it uses) if its egress is restricted.
