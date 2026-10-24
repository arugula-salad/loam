// Uses public, synthetic Postman Echo credentials; never a vendor API key.
import {Secret, Sandbox} from 'e2b'

const name = `loam-egress-test-${Date.now()}`
const value = Buffer.from('postman:password').toString('base64')
let box
let secret
try {
  secret = await Secret.create(name, value, {metadata: {test: 'loam-egress'}})
  if (secret.version !== 1 || JSON.stringify(secret).includes(value)) throw new Error('secret create leaked a value or wrong version')
  const info = await Secret.getInfo(name)
  if (info.secretId !== secret.secretId) throw new Error('secret lookup by name')
  box = await Sandbox.create({
    network: {
      denyOut: ['0.0.0.0/0'], allowOut: ['postman-echo.com'],
      rules: {'postman-echo.com': [{transform: {headers: {
        Authorization: `Basic ${Secret.fill(name)}`,
      }}}]},
    },
  })
  const status = async path => (await box.commands.run(`curl --max-time 30 -sS -o /dev/null -w '%{http_code}' -H 'Authorization: guest-value' https://postman-echo.com/${path}`)).stdout.trim()
  if (await status('basic-auth') !== '200') throw new Error('broker did not replace guest authorization')
  if (await status('headers') !== '502') throw new Error('broker allowed credential reflection')
  const env = await box.commands.run('env; cat /proc/1/environ')
  if (env.stdout.includes(value)) throw new Error('raw credential reached guest environment')
  const updated = await Secret.update(name, 'invalid-synthetic-credential')
  if (updated.version !== 2 || await status('basic-auth') !== '401') throw new Error('rotation was not applied')
  await box.betaPause()
  await Secret.update(name, value)
  box = await Sandbox.connect(box.sandboxId)
  if (await status('basic-auth') !== '200') throw new Error('resume did not resolve current secret')
  await Secret.destroy(name)
  secret = undefined
  if (await status('basic-auth') !== '502') throw new Error('deleted credential did not fail closed')
  console.log('SDK secret create/read/rotate/pause/resume/delete and broker isolation passed')
} finally {
  if (box) await box.kill()
  if (secret) await Secret.destroy(secret.secretId)
}
