import subprocess,json,time,selectors,os
from pathlib import Path
start=time.monotonic()
d=Path('/home/sprite/.local/share/managoat/acp/claude-agent-acp@0.81.2')
assert (d/'.installed').read_text()=='0.81.2'
assert os.readlink('/home/sprite/.local/bin/claude-agent-acp')==str(d/'launch')
assert os.access(d/'launch',os.X_OK)
cli=Path((d/'.claude-executable').read_text().strip())
assert cli.is_absolute() and os.access(cli,os.X_OK) and not cli.is_relative_to(d)
assert not list((d/'node_modules/@anthropic-ai').glob('claude-agent-sdk-*'))
assert os.access((d/'.node').read_text().strip(),os.X_OK)
assert json.loads((d/'node_modules/@anthropic-ai/claude-agent-sdk/package.json').read_text())['version']=='0.3.280'
print('Fountain cache check: hit; %.3f ms'%((time.monotonic()-start)*1000),flush=True)
p=subprocess.Popen([str(d/'launch')],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
sel=selectors.DefaultSelector();sel.register(p.stdout,selectors.EVENT_READ)
try:
 for i,method,params in [(1,'initialize',{'protocolVersion':1,'clientCapabilities':{},'clientInfo':{'name':'loam-image-test','version':'1'}}),(2,'session/new',{'cwd':'/home/sprite','mcpServers':[]})]:
  p.stdin.write(json.dumps({'jsonrpc':'2.0','id':i,'method':method,'params':params})+'\n');p.stdin.flush()
  deadline=time.monotonic()+30
  while time.monotonic()<deadline:
   if not sel.select(timeout=1): continue
   line=p.stdout.readline()
   if not line: raise RuntimeError('adapter exited: '+p.stderr.read()[:1000])
   msg=json.loads(line)
   if msg.get('id')!=i: continue
   if 'error' in msg: raise RuntimeError(msg['error'])
   print(method+': OK',flush=True);break
  else: raise RuntimeError(method+' timed out')
finally:
 p.terminate();p.wait(timeout=10)
