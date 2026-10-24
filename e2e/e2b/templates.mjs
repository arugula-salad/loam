// Run against an isolated deployment with the same E2B_* settings as js.mjs.
import {Template, Sandbox} from 'e2b'
import {mkdtemp, writeFile, rm} from 'node:fs/promises'
import {tmpdir} from 'node:os'
import {join} from 'node:path'

const context = await mkdtemp(join(tmpdir(), 'loam-template-'))
const name = `loam-template-test-${Date.now()}`
const boxes = []
let built
try {
  await writeFile(join(context, 'hello.txt'), 'copied-by-sdk\n')
  const template = Template({fileContextPath: context})
    .fromTemplate('base')
    .copy('hello.txt', '/tmp/from-build.txt')
    .setEnvs({LOAM_BUILD_VALUE: 'persisted'})
    .setUser('user')
    .setWorkdir('/home/user')
    .setStartCmd('echo $$ > /tmp/template-pid; touch /tmp/template-ready; sleep 900', 'test -f /tmp/template-ready')
  built = await Template.build(template, name, {
    tags: ['v1'], cpuCount: 2, memoryMB: 1024,
    onBuildLogs: entry => console.log(entry.toString()),
  })
  for (let n = 0; n < 2; n++) {
    const start = Date.now()
    const box = await Sandbox.create(`${name}:v1`)
    boxes.push(box)
    const result = await box.commands.run('set -e; test "$LOAM_BUILD_VALUE" = persisted; test "$(id -un)" = user; test "$PWD" = /home/user; test "$(cat /tmp/from-build.txt)" = copied-by-sdk; kill -0 $(cat /tmp/template-pid); cat /run/e2b/.E2B_SANDBOX_ID')
    if (result.stdout.trim() !== box.sandboxId) throw new Error('snapshot retained its source identity')
    console.log(`Snapshot create + validation: ${Date.now() - start} ms`)
  }
} finally {
  await Promise.all(boxes.map(box => box.kill()))
  if (built) {
    const url = process.env.E2B_API_URL ?? `https://api.${process.env.E2B_DOMAIN}`
    const response = await fetch(`${url}/templates/${built.templateId}`, {
      method: 'DELETE', headers: {'X-API-Key': process.env.E2B_API_KEY},
    })
    if (!response.ok) throw new Error(`template cleanup: ${response.status}`)
  }
  await rm(context, {recursive: true, force: true})
}
