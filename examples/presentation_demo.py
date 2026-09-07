"""Exercise a real local approval round trip without launching a coding agent."""
import concurrent.futures
import json
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
TOKEN = 'local-demo-only'
ENVELOPE = {'id':'demo-approval','agent_id':'demo-client','prompt':'Allow reading demo.txt?',
            'choices':[{'key':'y','label':'Approve','is_default':False},
                       {'key':'n','label':'Deny','is_default':True}]}
with tempfile.TemporaryDirectory(prefix='agentq-demo-') as tmp:
    binary = str(Path(tmp) / 'agentq')
    subprocess.run(['go','build','-o',binary,'./cmd/agentq'],cwd=ROOT,check=True)
    with socket.socket() as sock:
        sock.bind(('127.0.0.1',0)); port=sock.getsockname()[1]
    base=f'http://127.0.0.1:{port}'
    def request(route,body=None):
        data=json.dumps(body).encode() if body is not None else None
        req=urllib.request.Request(base+route,data=data,headers={'Authorization':'Bearer '+TOKEN,'Content-Type':'application/json'})
        with urllib.request.urlopen(req,timeout=10) as response:
            return json.load(response)
    process=subprocess.Popen([binary,'serve','--listen',f'127.0.0.1:{port}','--data-dir',tmp,'--token',TOKEN],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
    try:
        for attempt in range(100):
            try:
                request('/healthz');break
            except OSError:
                if process.poll() is not None:raise RuntimeError(process.communicate())
                time.sleep(.05)
        else:raise RuntimeError('Local daemon did not start')
        before=len(request('/api/queue'))
        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
            pending=pool.submit(request,'/api/envelopes',ENVELOPE)
            for attempt in range(100):
                queue=request('/api/queue')
                if queue:break
                time.sleep(.05)
            else:raise RuntimeError('Envelope never entered queue')
            request('/api/queue/demo-approval/answer',{'choice_key':'n'})
            answer=pending.result(timeout=10)
        result={'pending_before':before,'pending_during':[item['id'] for item in queue],
                'answer':{key:answer[key] for key in ['envelope_id','choice_key']},
                'pending_after':len(request('/api/queue'))}
        print(json.dumps(result,indent=2))
        assert result['pending_before']==result['pending_after']==0
        assert result['answer']['choice_key']=='n'
    finally:
        process.terminate()
        process.communicate(timeout=8)
