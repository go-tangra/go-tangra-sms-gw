#!/usr/bin/env python3
"""Record the running legacy v3 gateway against isolated PostgreSQL and mock carriers.

The legacy checkout (../go-tangra-sms-gw) is built read-only into a temporary
directory. PostgreSQL runs in a throwaway container on a TEST-NET-2 docker
network (198.51.100.0/24) so the legacy webhook dialer, which refuses private
and loopback addresses, can reach the in-process callback receiver bound on the
bridge address. Carriers are the legacy test-linkmobility mock plus an
in-process fake for failure modes. Nothing contacts a real carrier.

Run: sg docker -c "python3 scripts/capture_legacy_runtime.py"
Output: tests/fixtures/legacy/runtime/*.json (normalized, no secrets of value).
With --snapshot PATH the fixtures stay untouched and the populated legacy
database is dumped to PATH instead (migration rehearsal source).
"""
import base64
import concurrent.futures
import gzip
import hashlib
import hmac
import http.client
import http.server
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT.parent / 'go-tangra-sms-gw'
OUT = ROOT / 'tests/fixtures/legacy/runtime'
GO = os.environ.get('GO', 'go')
NET = 'smsgw-capture-net'
SUBNET = '198.51.100.0/24'
BRIDGE_IP = '198.51.100.1'
PG_IP = '198.51.100.10'
PG = 'smsgw-capture-pg'
JWT_SECRET = 'capture-only-jwt-secret-0123456789abcdef'
CALLBACK_SECRET = 'capture-callback-secret-not-production'
CARRIER_TOKEN = 'capture-carrier-token-not-production'
T_AUTO = 'a' * 32
T_MANUAL = 'b' * 32
T_SLOW = 'c' * 32
FIXED_TS = 1700000000

UUID_RE = re.compile(r'[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}')
RFC3339_RE = re.compile(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})')
JWT_RE = re.compile(r'eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*')
HTTP_DATE_RE = re.compile(r'(Date: )[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT')


def run(cmd, **kw):
    return subprocess.run(cmd, check=True, text=True, capture_output=True, **kw).stdout


def free_port(host='127.0.0.1'):
    with socket.socket() as s:
        s.bind((host, 0))
        return s.getsockname()[1]


def b64url(data):
    return base64.urlsafe_b64encode(data).rstrip(b'=').decode()


def forge_jwt(claims, secret=JWT_SECRET, alg='HS256'):
    head = b64url(json.dumps({'alg': alg, 'typ': 'JWT'}, separators=(',', ':')).encode())
    body = b64url(json.dumps(claims, separators=(',', ':')).encode())
    if alg == 'none':
        return f'{head}.{body}.'
    sig = hmac.new(secret.encode(), f'{head}.{body}'.encode(), hashlib.sha256).digest()
    return f'{head}.{body}.{b64url(sig)}'


def jwt_parts(tok):
    head, body, _ = tok.split('.')
    pad = lambda s: s + '=' * (-len(s) % 4)
    return (json.loads(base64.urlsafe_b64decode(pad(head))),
            json.loads(base64.urlsafe_b64decode(pad(body))))


class Normalizer:
    """Replaces nondeterministic values by stable named placeholders."""

    def __init__(self):
        self.names = {}

    def name(self, value, label):
        if value and value not in self.names:
            self.names[value] = '{{' + label + '}}'
        return self.names.get(value, value)

    def __call__(self, text):
        if text is None:
            return None
        for value in sorted(self.names, key=len, reverse=True):
            text = text.replace(value, self.names[value])
        text = JWT_RE.sub('{{unregistered_jwt}}', text)
        text = UUID_RE.sub('{{unregistered_uuid}}', text)
        text = RFC3339_RE.sub('{{rfc3339}}', text)
        return HTTP_DATE_RE.sub(r'\1{{http_date}}', text)


NORM = Normalizer()
NORM.names['00000000-0000-0000-0000-000000000000'] = '00000000-0000-0000-0000-000000000000'


class FakeCarrier(http.server.ThreadingHTTPServer):
    """Failure-mode carrier: /reject, /http500, /drop, /slow."""

    def __init__(self, addr):
        self.slow_entered = threading.Event()
        self.slow_release = threading.Event()
        self.requests = []
        super().__init__(addr, FakeCarrierHandler)


class FakeCarrierHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *a):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
        self.server.requests.append({'path': self.path, 'body': body.decode()})
        if self.path.startswith('/drop'):
            self.connection.shutdown(socket.SHUT_RDWR)
            self.close_connection = True
            return
        if self.path.startswith('/http500'):
            return self.reply(500, b'{"error":"carrier exploded"}')
        if self.path.startswith('/reject'):
            return self.reply(200, b'{"return_code":2001,"return_message":"Invalid SID"}')
        if self.path.startswith('/slow'):
            self.server.slow_entered.set()
            self.server.slow_release.wait(30)
            return self.reply(200, b'{"return_code":0,"return_message":"Message accepted","channels":{"sms":{"send_order":1,"message_parts":1}}}')
        self.reply(404, b'{}')

    def reply(self, code, body):
        self.send_response(code)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class Receiver(http.server.ThreadingHTTPServer):
    def __init__(self, addr):
        self.hits = []
        self.lock = threading.Lock()
        super().__init__(addr, ReceiverHandler)


class ReceiverHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *a):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
        with self.server.lock:
            self.server.hits.append({'at': time.monotonic(), 'path': self.path, 'headers': dict(self.headers), 'body': body})
        if self.path.startswith('/fail'):
            code, extra = 500, []
        elif self.path.startswith('/redirect'):
            code, extra = 302, [('Location', f'http://{BRIDGE_IP}:{self.server.server_address[1]}/ok')]
        else:
            code, extra = 200, []
        self.send_response(code)
        for k, v in extra:
            self.send_header(k, v)
        self.send_header('Content-Length', '0')
        self.end_headers()


class Capture:
    def __init__(self, tmp):
        self.tmp = tmp
        self.cases = {}
        self.procs = []
        self.gw = None

    # ---------- infrastructure ----------

    def build(self):
        env = dict(os.environ, CGO_ENABLED='0', GOWORK='off', GOFLAGS='-mod=readonly')
        env.pop('GOROOT', None)
        for target, pkg in (('sms-gw-server', './cmd/server'), ('test-linkmobility', './cmd/test-linkmobility')):
            subprocess.run([GO, 'build', '-o', str(self.tmp / target), pkg], cwd=SOURCE, env=env, check=True)
        self.source_rev = run(['git', '-C', str(SOURCE), 'rev-parse', 'HEAD']).strip()
        self.source_dirty = bool(run(['git', '-C', str(SOURCE), 'status', '--porcelain']).strip())

    def certs(self):
        d = self.tmp / 'certs'
        for sub in ('ca', 'server', 'client'):
            (d / sub).mkdir(parents=True)
        ssl = lambda *a: run(['openssl', *a])
        ssl('req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes', '-keyout', str(d / 'ca/ca.key'),
            '-out', str(d / 'ca/ca.crt'), '-days', '365', '-subj', '/CN=capture-ca')
        ssl('req', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes', '-keyout', str(d / 'server/server.key'),
            '-out', str(d / 'server.csr'), '-subj', '/CN=sms-gw-service')
        (d / 'ext.cnf').write_text('extendedKeyUsage=serverAuth,clientAuth\nsubjectAltName=IP:127.0.0.1\n')
        ssl('x509', '-req', '-in', str(d / 'server.csr'), '-CA', str(d / 'ca/ca.crt'), '-CAkey', str(d / 'ca/ca.key'),
            '-CAcreateserial', '-out', str(d / 'server/server.crt'), '-days', '200', '-extfile', str(d / 'ext.cnf'))
        shutil.copy(d / 'server/server.crt', d / 'client/client.crt')
        shutil.copy(d / 'server/server.key', d / 'client/client.key')
        der = subprocess.run(['openssl', 'x509', '-in', str(d / 'ca/ca.crt'), '-outform', 'DER'], check=True, capture_output=True).stdout
        self.ca_fp = hashlib.sha256(der).hexdigest()

    def database(self):
        subprocess.run(['docker', 'rm', '-f', PG], capture_output=True)
        subprocess.run(['docker', 'network', 'rm', NET], capture_output=True)
        run(['docker', 'network', 'create', '--subnet', SUBNET, NET])
        run(['docker', 'run', '-d', '--name', PG, '--network', NET, '--ip', PG_IP,
             '-e', 'POSTGRES_PASSWORD=capture', '-e', 'POSTGRES_DB=sms_gw', 'postgres:16'])
        for _ in range(60):
            if subprocess.run(['docker', 'exec', PG, 'pg_isready', '-U', 'postgres', '-d', 'sms_gw'], capture_output=True).returncode == 0:
                time.sleep(1)
                return
            time.sleep(0.5)
        raise RuntimeError('postgres did not start')

    def sql(self, statement):
        return run(['docker', 'exec', '-i', PG, 'psql', '-U', 'postgres', '-d', 'sms_gw', '-At', '-v', 'ON_ERROR_STOP=1', '-c', statement]).strip()

    def sql_json(self, statement):
        out = self.sql(f'SELECT coalesce(json_agg(t), \'[]\') FROM ({statement}) t')
        return json.loads(out)

    def start_services(self):
        self.carrier = FakeCarrier(('127.0.0.1', 0))
        self.receiver = Receiver((BRIDGE_IP, 0))
        for srv in (self.carrier, self.receiver):
            threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.mock_auto = free_port()
        self.mock_manual = free_port()
        for port, extra in ((self.mock_auto, ['--dlr-delay=300ms']), (self.mock_manual, ['--no-dlr-rate=1'])):
            p = subprocess.Popen([str(self.tmp / 'test-linkmobility'), f'--addr=127.0.0.1:{port}', *extra],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env={'SMSGW_TEST_SEED': '1'})
            self.procs.append(p)
            self.wait_http(port, '/healthz')
        self.gw_port = free_port()
        self.grpc_port = free_port()

    def start_gateway(self, limits):
        cfg = self.tmp / 'configs'
        if not cfg.exists():
            shutil.copytree(SOURCE / 'configs', cfg)
            data = (cfg / 'data.yaml').read_text().replace('postgresql://postgres:admin123@localhost:5432/sms_gw',
                                                          f'postgresql://postgres:capture@{PG_IP}:5432/sms_gw')
            (cfg / 'data.yaml').write_text(data)
            srv = (cfg / 'server.yaml').read_text().replace('0.0.0.0:9900', f'127.0.0.1:{self.grpc_port}').replace('0.0.0.0:9901', f'127.0.0.1:{free_port()}')
            (cfg / 'server.yaml').write_text(srv)
        env = {
            'PATH': os.environ['PATH'], 'HOME': str(self.tmp),
            'CERTS_DIR': str(self.tmp / 'certs'), 'LCM_CA_FINGERPRINT': self.ca_fp,
            'LCM_BOOTSTRAP_ENDPOINT': '127.0.0.1:1', 'MODULE_BOOTSTRAP_SECRET': 'unused-capture',
            'SMS_GW_JWT_SECRET': JWT_SECRET, 'HTTP_PUBLIC_BIND': f'127.0.0.1:{self.gw_port}',
            'SMS_GW_ALLOW_HTTP_WEBHOOKS': '1',
        }
        if limits == 'relaxed':
            for k in ('LOGIN', 'SEND', 'DLR'):
                env[f'SMS_GW_{k}_RPM'] = '1000000'
                env[f'SMS_GW_{k}_BURST'] = '100000'
        log = open(self.tmp / f'gateway-{limits}.log', 'w')
        self.gw = subprocess.Popen([str(self.tmp / 'sms-gw-server'), '-c', str(cfg)], cwd=self.tmp, env=env, stdout=log, stderr=log)
        self.wait_http(self.gw_port, '/health')

    def stop_gateway(self):
        if self.gw:
            self.gw.terminate()
            self.gw.wait(15)
            self.gw = None

    def wait_http(self, port, path):
        for _ in range(100):
            try:
                c = http.client.HTTPConnection('127.0.0.1', port, timeout=2)
                c.request('GET', path)
                if c.getresponse().status == 200:
                    return
            except OSError:
                pass
            time.sleep(0.2)
        raise RuntimeError(f'service on {port} did not become healthy')

    def teardown(self):
        self.stop_gateway()
        for p in self.procs:
            p.terminate()
        subprocess.run(['docker', 'rm', '-f', PG], capture_output=True)
        subprocess.run(['docker', 'network', 'rm', NET], capture_output=True)

    # ---------- HTTP ----------

    def raw(self, method, path, body=None, headers=None, port=None):
        c = http.client.HTTPConnection('127.0.0.1', port or self.gw_port, timeout=60)
        h = dict(headers or {})
        if isinstance(body, (dict, list)):
            body = json.dumps(body)
            h.setdefault('Content-Type', 'application/json')
        c.request(method, path, body=body, headers=h)
        r = c.getresponse()
        data = r.read().decode('utf-8', 'replace')
        return r.status, r.getheader('Content-Type') or '', data

    def call(self, case, method, path, body=None, headers=None, note=None, observe=None):
        status, ctype, data = self.raw(method, path, body, headers)
        rec = {
            'case': case,
            'request': {'method': method, 'path': NORM(path), 'headers': {k: NORM(v) for k, v in (headers or {}).items()},
                        'body': NORM(body if isinstance(body, str) or body is None else json.dumps(body))},
            'response': {'status': status, 'content_type': ctype, 'body': data},
        }
        if note:
            rec['note'] = note
        if observe:
            rec['observed'] = observe
        self.cases[case] = rec
        try:
            return status, json.loads(data) if data else None
        except ValueError:
            return status, data

    def observe(self, case, key, value):
        self.cases[case].setdefault('observed', {})[key] = value

    def login(self, user, pw='Passw0rd!'):
        _, _, data = self.raw('POST', '/hermes/v1/login', {'username': user, 'password': pw, 'grant_type': 'password'})
        out = json.loads(data)
        NORM.name(out['access_token'], f'access_token:{user}')
        NORM.name(out['refresh_token'], f'refresh_token:{user}')
        return out

    def bearer(self, tok):
        return {'Authorization': 'Bearer ' + tok}

    # ---------- seed ----------

    def seed(self):
        self.schema = run(['docker', 'exec', PG, 'pg_dump', '-U', 'postgres', '-s', '--no-owner', '--no-privileges', 'sms_gw'])
        self.sql('CREATE EXTENSION IF NOT EXISTS pgcrypto')
        gw = f'http://127.0.0.1:{self.gw_port}'
        rcv = f'http://{BRIDGE_IP}:{self.receiver.server_address[1]}'
        carrier = f'http://127.0.0.1:{self.carrier.server_address[1]}'
        clients = [
            ('client_a', 'API_CLIENT', 'ON', f'{rcv}/ok/a', CALLBACK_SECRET),
            ('client_b', 'API_CLIENT', 'ON', f'{rcv}/ok/b', ''),
            ('viewer_v', 'API_VIEWER', 'ON', '', ''),
            ('admin_x', 'API_ADMIN', 'ON', '', ''),
            ('disabled_d', 'API_CLIENT', 'OFF', '', ''),
            ('client_f', 'API_CLIENT', 'ON', f'{rcv}/fail/f', CALLBACK_SECRET),
            ('client_r', 'API_CLIENT', 'ON', f'{rcv}/redirect/r', ''),
            ('client_l', 'API_CLIENT', 'ON', '', ''),
            ('client_s', 'API_CLIENT', 'ON', '', ''),
        ]
        for user, auth, status, url, secret in clients:
            self.sql(f"""INSERT INTO sms_api_client (create_time, update_time, username, password_hash, email, authority, status,
                last_login_ip, dlr_callback_url, dlr_callback_secret)
                VALUES ('2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','{user}', crypt('Passw0rd!', gen_salt('bf', 4)),
                '{user}@example.invalid','{auth}','{status}','','{url}','{secret}')""")
        self.client_id = dict((u, int(self.sql(f"SELECT id FROM sms_api_client WHERE username='{u}'"))) for u, *_ in clients)

        def provider(name, ptype, cfg, obj='OBJECT_TYPE_SMS', status='ON'):
            return int(self.sql(f"""INSERT INTO sms_provider (create_time, update_time, name, type, object_type, config, status, retention_days)
                VALUES ('2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','{name}','{ptype}','{obj}','{json.dumps(cfg)}','{status}',0) RETURNING id""").splitlines()[0])

        mock = lambda port: f'http://127.0.0.1:{port}/multichannel-api/sendmulti/'
        base = {'sid': '9999', 'encoding': 'utf-8', 'priority': '2'}
        self.p = {
            'auto': provider('mock-auto', 'voicecom', {**base, 'url': mock(self.mock_auto), 'token': CARRIER_TOKEN,
                                                     'callback_url': f'{gw}/dlr?dlr_token={T_AUTO}', 'dlr_token': T_AUTO}),
            'manual': provider('mock-manual', 'voicecom', {**base, 'url': mock(self.mock_manual),
                                                         'callback_url': f'{gw}/dlr?dlr_token={T_MANUAL}', 'dlr_token': T_MANUAL}),
            'legacy': provider('mock-legacy-no-token', 'voicecom', {**base, 'url': mock(self.mock_manual), 'callback_url': f'{gw}/dlr'}),
            'off': provider('mock-off', 'voicecom', {**base, 'url': mock(self.mock_manual), 'callback_url': f'{gw}/dlr'}, status='OFF'),
            'viber': provider('mock-viber', 'voicecom', {**base, 'url': mock(self.mock_manual), 'callback_url': f'{gw}/dlr'}, obj='OBJECT_TYPE_VIBER'),
            'reject': provider('fake-reject', 'voicecom', {**base, 'url': f'{carrier}/reject', 'callback_url': f'{gw}/dlr'}),
            'http500': provider('fake-http500', 'voicecom', {**base, 'url': f'{carrier}/http500', 'callback_url': f'{gw}/dlr'}),
            'drop': provider('fake-drop', 'voicecom', {**base, 'url': f'{carrier}/drop', 'callback_url': f'{gw}/dlr'}),
            'slow': provider('fake-slow', 'voicecom', {**base, 'url': f'{carrier}/slow', 'callback_url': f'{gw}/dlr?dlr_token={T_SLOW}', 'dlr_token': T_SLOW}),
            'unknown': provider('unknown-type', 'carrier-x', {'url': f'{carrier}/reject'}),
            'nocallback': provider('mock-no-callback', 'voicecom', {**base, 'url': mock(self.mock_manual)}),
        }

        def template(name, templates, obj='OBJECT_TYPE_SMS', status='ON'):
            return int(self.sql(f"""INSERT INTO sms_template (create_time, update_time, name, object_type, templates, status)
                VALUES ('2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','{name}','{obj}','{json.dumps(templates, ensure_ascii=False)}','{status}') RETURNING id""").splitlines()[0])

        self.t = {
            'hello': template('hello', {'body': 'Hello {{ .name }}, code {{ .code }}.'}),
            'static': template('static', {'body': 'ping'}),
            'off': template('off', {'body': 'off'}, status='OFF'),
            'viber': template('viber', {'body': 'viber'}, obj='OBJECT_TYPE_VIBER'),
            'nobody': template('nobody', {'subject': 'no body fragment'}),
            'badsyntax': template('badsyntax', {'body': 'Hello {{ .name'}),
            'unicode': template('unicode', {'body': 'Здравей {{ .name }}'}),
        }
        for recipient, pid, status in (('359888000301', self.p['manual'], 'ON'), ('359888000302', 0, 'ON'), ('359888000303', self.p['manual'], 'OFF')):
            self.sql(f"""INSERT INTO sms_block (create_time, update_time, recipient, description, provider_id, block_type, status)
                VALUES ('2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','{recipient}','capture','{pid}','OBJECT_TYPE_SMS','{status}')""")

    def send_body(self, provider, template='static', to=359888000100, props=None, sms=None):
        return {'to': to, 'providerId': self.p[provider] if isinstance(provider, str) else provider,
                'templateId': self.t[template] if isinstance(template, str) else template,
                'properties': props or {}, 'sms': sms or {'from': 'Capture', 'encoding': 'utf-8'}}

    def message_row(self, mid):
        rows = self.sql_json(f"""SELECT id, create_by, sid, recipient, priority, provider_id, template_id, defer, user_name, data, dlr_ts,
            status_code, message, status_message, remote_address,
            encode(raw_request, 'base64') AS raw_request, encode(raw_response, 'base64') AS raw_response
            FROM sms_message WHERE id = '{mid}'""")
        if not rows:
            return None
        row = rows[0]
        for k in ('raw_request', 'raw_response'):
            if row[k]:
                blob = base64.b64decode(row[k].replace('\n', ''))
                try:
                    blob = gzip.decompress(blob)
                    row[k + '_storage'] = 'gzip'
                except OSError:
                    row[k + '_storage'] = 'plain'
                row[k] = blob.decode('utf-8', 'replace')
        return row

    def dlr_rows(self, mid):
        return self.sql_json(f"""SELECT id, channel, sid, status_text, message_status, recipient, sender, "timestamp", remote_address, parts_received
            FROM sms_dlr WHERE message_id = '{mid}' ORDER BY id""")

    def dlr(self, case, params, path='/dlr', method='GET', note=None):
        q = urllib.parse.urlencode(params)
        return self.call(case, method, f'{path}?{q}', note=note)

    # ---------- scenarios ----------

    def scenarios(self):
        self.surface()
        self.auth()
        self.sends()
        self.carrier_failures()
        self.early_dlr()
        self.reads()
        self.receipts()
        self.concurrency()
        self.logout()

    def surface(self):
        self.call('surface-health', 'GET', '/health')
        self.call('surface-unknown-route', 'GET', '/hermes/v1/unknown')
        self.call('surface-wrong-method', 'DELETE', '/hermes/v1/sms')
        status, ctype, body = self.raw('GET', '/')
        self.cases['surface-root'] = {'case': 'surface-root', 'request': {'method': 'GET', 'path': '/'},
                                      'response': {'status': status, 'content_type': ctype, 'body': '<embedded frontend omitted>' if status == 200 else body}}

    def auth(self):
        creds = {'username': 'client_a', 'password': 'Passw0rd!', 'grant_type': 'password'}
        status, out = self.call('login-json', 'POST', '/hermes/v1/login', creds)
        NORM.name(out['access_token'], 'access_token:client_a')
        NORM.name(out['refresh_token'], 'refresh_token:client_a')
        self.tok_a = out
        ah, ac = jwt_parts(out['access_token'])
        rh, rc = jwt_parts(out['refresh_token'])
        NORM.name(ac['jti'], 'jti:client_a')

        def claims(c):
            c = dict(c)
            c['exp_minus_iat'] = c['exp'] - c['iat']
            c['nbf_equals_iat'] = c['nbf'] == c['iat']
            for k in ('exp', 'iat', 'nbf'):
                c[k] = '{{unix_seconds}}'
            c['jti'] = NORM(c['jti'])
            return c
        self.cases['jwt-claims'] = {'case': 'jwt-claims', 'note': 'Decoded from login-json tokens; access and refresh share one jti',
                                    'observed': {'access': {'header': ah, 'claims': claims(ac)}, 'refresh': {'header': rh, 'claims': claims(rc)},
                                                 'same_jti': ac['jti'] == rc['jti'], 'jti_hex_len': len(ac['jti'])}}
        form = urllib.parse.urlencode({'username': 'client_b', 'password': 'Passw0rd!', 'grant_type': 'password'})
        status, out = self.call('login-form', 'POST', '/hermes/v1/login', form, {'Content-Type': 'application/x-www-form-urlencoded'})
        if status == 200:
            NORM.name(out['access_token'], 'access_token:client_b(form)')
            NORM.name(out['refresh_token'], 'refresh_token:client_b(form)')
        self.call('login-missing-password', 'POST', '/hermes/v1/login', {'username': 'client_a', 'grant_type': 'password'})
        self.call('login-empty-body', 'POST', '/hermes/v1/login', {})
        self.call('login-unknown-user', 'POST', '/hermes/v1/login', {'username': 'nobody', 'password': 'x', 'grant_type': 'password'})
        self.call('login-wrong-password', 'POST', '/hermes/v1/login', {'username': 'client_a', 'password': 'wrong', 'grant_type': 'password'})
        self.call('login-disabled', 'POST', '/hermes/v1/login', {'username': 'disabled_d', 'password': 'Passw0rd!', 'grant_type': 'password'})
        self.call('login-malformed-json', 'POST', '/hermes/v1/login', '{"username":', {'Content-Type': 'application/json'})
        self.call('login-without-grant-type', 'POST', '/hermes/v1/login', {'username': 'client_l', 'password': 'Passw0rd!'})
        last = self.sql_json("SELECT client_id, username, success, error_message, login_ip, user_agent FROM sms_login_log ORDER BY id")
        self.cases['login-log-rows'] = {'case': 'login-log-rows', 'note': 'sms_login_log rows after the login cases above, in order',
                                        'observed': {'rows': last,
                                                     'last_login': self.sql_json("SELECT username, last_login_ip, last_login_time IS NOT NULL AS has_time FROM sms_api_client ORDER BY id")}}
        self.tok_b = self.login('client_b')
        self.tok_v = self.login('viewer_v')
        self.tok_x = self.login('admin_x')
        self.tok_f = self.login('client_f')
        self.tok_r = self.login('client_r')

        a = self.tok_a['access_token']
        self.call('me-client', 'GET', '/hermes/v1/me', headers=self.bearer(a))
        self.call('me-viewer', 'GET', '/hermes/v1/me', headers=self.bearer(self.tok_v['access_token']))
        self.call('me-admin', 'GET', '/hermes/v1/me', headers=self.bearer(self.tok_x['access_token']))
        self.call('me-missing-token', 'GET', '/hermes/v1/me')
        self.call('me-non-bearer-scheme', 'GET', '/hermes/v1/me', headers={'Authorization': 'Basic Zm9vOmJhcg=='})
        self.call('me-lowercase-bearer', 'GET', '/hermes/v1/me', headers={'Authorization': 'bearer ' + a})
        self.call('me-garbage-token', 'GET', '/hermes/v1/me', headers=self.bearer('not-a-jwt'))
        self.call('me-refresh-token-as-access', 'GET', '/hermes/v1/me', headers=self.bearer(self.tok_a['refresh_token']))
        now = int(time.time())
        cid = self.client_id['client_a']
        base = {'authority': 'API_CLIENT', 'kind': 'access', 'username': 'client_a', 'sub': str(cid), 'iss': 'sms-gw', 'jti': 'f' * 32}
        forged = {
            'me-expired-access': forge_jwt({**base, 'iat': now - 7300, 'nbf': now - 7300, 'exp': now - 100}),
            'me-not-yet-valid': forge_jwt({**base, 'iat': now, 'nbf': now + 3600, 'exp': now + 7200}),
            'me-wrong-secret': forge_jwt({**base, 'iat': now, 'nbf': now, 'exp': now + 7200}, secret='another-secret-another-secret-123'),
            'me-alg-none': forge_jwt({**base, 'iat': now, 'nbf': now, 'exp': now + 7200}, alg='none'),
            'me-missing-kind': forge_jwt({k: v for k, v in {**base, 'iat': now, 'nbf': now, 'exp': now + 7200}.items() if k != 'kind'}),
            'me-unknown-client-id': forge_jwt({**base, 'sub': '999999', 'iat': now, 'nbf': now, 'exp': now + 7200}),
            'me-non-numeric-sub': forge_jwt({**base, 'sub': 'abc', 'iat': now, 'nbf': now, 'exp': now + 7200}),
        }
        for case, tok in forged.items():
            NORM.name(tok, 'forged_jwt:' + case)
            self.call(case, 'GET', '/hermes/v1/me', headers=self.bearer(tok), note='Token forged with the capture secret (see case name)')
        self.unknown_authority = forge_jwt({**base, 'authority': 'ROOT', 'iat': now, 'nbf': now, 'exp': now + 7200})
        NORM.name(self.unknown_authority, 'forged_jwt:authority-ROOT')
        self.call('me-unknown-authority', 'GET', '/hermes/v1/me', headers=self.bearer(self.unknown_authority))

        status, out = self.call('refresh-json', 'POST', '/hermes/v1/refresh_token', {'refresh_token': self.tok_a['refresh_token'], 'grant_type': 'refresh_token'})
        if status == 200:
            NORM.name(out['access_token'], 'access_token:client_a(refreshed)')
            NORM.name(out['refresh_token'], 'refresh_token:client_a(refreshed)')
            self.observe('refresh-json', 'new_jti_differs', jwt_parts(out['access_token'])[1]['jti'] != ac['jti'])
            self.observe('refresh-json', 'old_refresh_still_usable', self.raw('POST', '/hermes/v1/refresh_token', {'refresh_token': self.tok_a['refresh_token']})[0])
        form = urllib.parse.urlencode({'refresh_token': self.tok_b['refresh_token'], 'grant_type': 'refresh_token'})
        status, out = self.call('refresh-form', 'POST', '/hermes/v1/refresh_token', form, {'Content-Type': 'application/x-www-form-urlencoded'})
        if status == 200:
            NORM.name(out['access_token'], 'access_token:client_b(refresh-form)')
            NORM.name(out['refresh_token'], 'refresh_token:client_b(refresh-form)')
        self.call('refresh-with-access-token', 'POST', '/hermes/v1/refresh_token', {'refresh_token': a})
        self.call('refresh-missing', 'POST', '/hermes/v1/refresh_token', {})
        self.call('refresh-garbage', 'POST', '/hermes/v1/refresh_token', {'refresh_token': 'garbage'})
        expired_refresh = forge_jwt({**base, 'kind': 'refresh', 'iat': now - 700000, 'nbf': now - 700000, 'exp': now - 10})
        NORM.name(expired_refresh, 'forged_jwt:expired-refresh')
        self.call('refresh-expired', 'POST', '/hermes/v1/refresh_token', {'refresh_token': expired_refresh})
        tok_d = forge_jwt({**base, 'kind': 'refresh', 'sub': str(self.client_id['disabled_d']), 'username': 'disabled_d', 'iat': now, 'nbf': now, 'exp': now + 600})
        NORM.name(tok_d, 'forged_jwt:refresh-disabled')
        self.call('refresh-disabled-account', 'POST', '/hermes/v1/refresh_token', {'refresh_token': tok_d})
        tok_u = forge_jwt({**base, 'kind': 'refresh', 'sub': '999999', 'iat': now, 'nbf': now, 'exp': now + 600})
        NORM.name(tok_u, 'forged_jwt:refresh-unknown-client')
        self.call('refresh-unknown-client', 'POST', '/hermes/v1/refresh_token', {'refresh_token': tok_u})
        tok_bigsub = forge_jwt({**base, 'kind': 'refresh', 'sub': '99999999999999999999', 'iat': now, 'nbf': now, 'exp': now + 600})
        NORM.name(tok_bigsub, 'forged_jwt:refresh-overflow-sub')
        self.call('refresh-overflow-subject', 'POST', '/hermes/v1/refresh_token', {'refresh_token': tok_bigsub})
        self.call('refresh-stale-authority-claim', 'POST', '/hermes/v1/refresh_token',
                  {'refresh_token': self.tok_v['refresh_token']},
                  note='Refresh reissues the authority claim from the old token, not from the database row')
        self.sql(f"UPDATE sms_api_client SET authority='API_CLIENT' WHERE username='viewer_v'")
        refreshed = json.loads(self.raw('POST', '/hermes/v1/refresh_token', {'refresh_token': self.tok_v['refresh_token']})[2])
        self.observe('refresh-stale-authority-claim', 'authority_after_db_change_to_API_CLIENT', jwt_parts(refreshed['access_token'])[1]['authority'])
        self.sql(f"UPDATE sms_api_client SET authority='API_VIEWER' WHERE username='viewer_v'")

    def sends(self):
        a = self.bearer(self.tok_a['access_token'])
        ok = self.send_body('manual', 'hello', props={'name': 'Alice', 'code': '8421'})
        status, out = self.call('send-hermes', 'POST', '/hermes/v1/sms', ok, a)
        self.m_send = out['data']['id']
        NORM.name(self.m_send, 'msg:send-hermes')
        self.observe('send-hermes', 'db_row', self.message_row(self.m_send))
        status, out = self.call('send-v1-alias', 'POST', '/v1/sms', self.send_body('manual', 'static', to=359888000101), a)
        NORM.name(out['data']['id'], 'msg:send-v1-alias')
        self.m_alias = out['data']['id']
        status, out = self.call('send-to-as-string', 'POST', '/hermes/v1/sms', {**self.send_body('manual', to=0), 'to': '359888000102'}, a)
        if status == 200:
            NORM.name(out['data']['id'], 'msg:send-to-as-string')
        snake = {'to': 359888000103, 'provider_id': self.p['manual'], 'template_id': self.t['static'], 'sms': {'from': 'Capture'}}
        status, out = self.call('send-proto-field-names', 'POST', '/hermes/v1/sms', snake, a)
        if status == 200:
            NORM.name(out['data']['id'], 'msg:send-proto-field-names')
        full = self.send_body('manual', 'unicode', to=359888000104, props={'name': 'Мария'},
                              sms={'from': 'Capture', 'encoding': 'gsm-03-38', 'concatenate': 3, 'validity': {'ttl': 60, 'units': 'minutes'}, 'mccmnc': 28401})
        status, out = self.call('send-all-sms-options', 'POST', '/hermes/v1/sms', full, a)
        if status == 200:
            NORM.name(out['data']['id'], 'msg:send-all-sms-options')
            self.observe('send-all-sms-options', 'db_row', self.message_row(out['data']['id']))
        status, out = self.call('send-missing-template-variable', 'POST', '/hermes/v1/sms', self.send_body('manual', 'hello', to=359888000105, props={'name': 'Bob'}), a)
        if status == 200:
            NORM.name(out['data']['id'], 'msg:send-missing-template-variable')
        status, out = self.call('send-unknown-field', 'POST', '/hermes/v1/sms', {**self.send_body('manual', to=359888000106), 'text': 'free text ignored', 'defer': '2030-01-01'}, a)
        if status == 200:
            NORM.name(out['data']['id'], 'msg:send-unknown-field')
        status, out = self.call('send-no-carrier-callback', 'POST', '/hermes/v1/sms', self.send_body('nocallback', to=359888000107), a)
        if status == 200:
            NORM.name(out['data']['id'], 'msg:send-no-carrier-callback')
        before = int(self.sql('SELECT count(*) FROM sms_message'))
        neg = {
            'send-viewer-forbidden': (self.send_body('manual'), self.bearer(self.tok_v['access_token'])),
            'send-admin-authority': (self.send_body('manual', to=359888000108), self.bearer(self.tok_x['access_token'])),
            'send-unknown-authority': (self.send_body('manual'), self.bearer(self.unknown_authority)),
            'send-no-token': (self.send_body('manual'), {}),
            'send-provider-zero': (self.send_body(0), a),
            'send-provider-missing': (self.send_body(99999), a),
            'send-provider-off': (self.send_body('off'), a),
            'send-provider-viber': (self.send_body('viber'), a),
            'send-template-zero': (self.send_body('manual', 0), a),
            'send-template-missing': (self.send_body('manual', 99999), a),
            'send-template-off': (self.send_body('manual', 'off'), a),
            'send-template-viber': (self.send_body('manual', 'viber'), a),
            'send-template-no-body': (self.send_body('manual', 'nobody'), a),
            'send-template-bad-syntax': (self.send_body('manual', 'badsyntax'), a),
            'send-to-zero': (self.send_body('manual', to=0), a),
            'send-to-short': (self.send_body('manual', to=123456), a),
            'send-to-too-long': (self.send_body('manual', to=18446744073709551615), a),
            'send-to-negative': ({**self.send_body('manual'), 'to': -1}, a),
            'send-blocked-provider': (self.send_body('manual', to=359888000301), a),
            'send-blocked-global': (self.send_body('manual', to=359888000302), a),
            'send-unknown-provider-type': (self.send_body('unknown'), a),
            'send-malformed-json': ('{"to":', {**a, 'Content-Type': 'application/json'}),
        }
        for case, (body, hdr) in neg.items():
            self.call(case, 'POST', '/hermes/v1/sms', body, hdr)
        after = int(self.sql('SELECT count(*) FROM sms_message'))
        self.cases['send-rejections-persisted'] = {'case': 'send-rejections-persisted', 'note': 'Rows created by the rejected sends above (unknown provider type and bad template syntax included)',
                                                   'observed': {'rows_created': after - before}}
        status, out = self.call('send-block-disabled-not-enforced', 'POST', '/hermes/v1/sms', self.send_body('manual', to=359888000303), a)
        if status == 200:
            NORM.name(out['data']['id'], 'msg:send-block-disabled')
        status, out = self.call('send-other-client', 'POST', '/hermes/v1/sms', self.send_body('manual', to=359888000109), self.bearer(self.tok_b['access_token']))
        self.m_b = out['data']['id']
        NORM.name(self.m_b, 'msg:client_b')

    def carrier_failures(self):
        a = self.bearer(self.tok_a['access_token'])
        for case, prov, to in (('send-carrier-reject', 'reject', 359888000201), ('send-carrier-http500', 'http500', 359888000202),
                               ('send-carrier-connection-drop', 'drop', 359888000203)):
            self.call(case, 'POST', '/hermes/v1/sms', self.send_body(prov, to=to), a)
            row = self.sql_json(f"SELECT id FROM sms_message WHERE recipient = '{to}'")
            if row:
                mid = row[0]['id']
                NORM.name(mid, 'msg:' + case)
                self.observe(case, 'db_row', self.message_row(mid))
                status, _, body = self.raw('GET', f'/hermes/v1/sms/{mid}', headers=a)
                self.observe(case, 'get_after', {'status': status, 'body': body})
            self.observe(case, 'rows_for_recipient', len(row))

    def early_dlr(self):
        a = self.bearer(self.tok_a['access_token'])
        result = {}
        th = threading.Thread(target=lambda: result.update(r=self.raw('POST', '/hermes/v1/sms', self.send_body('slow', to=359888000210), a)))
        th.start()
        if not self.carrier.slow_entered.wait(15):
            raise RuntimeError('slow carrier not reached')
        mid = self.sql_json("SELECT id FROM sms_message WHERE recipient = '359888000210'")[0]['id']
        NORM.name(mid, 'msg:slow-carrier')
        self.call('get-while-carrier-pending', 'GET', f'/hermes/v1/sms/{mid}', headers=a,
                  note='Message read while the carrier call is still in flight: initial status -1 over the wire')
        self.call('list-while-carrier-pending', 'GET', '/hermes/v1/sms?' + urllib.parse.urlencode({'query': json.dumps({'recipient': '359888000210'})}), headers=a)
        self.dlr('dlr-before-carrier-response', {'request_id': mid, 'channel': 'sms', 'sid': 9999, 'message_status': 1, 'to': 359888000210,
                                                 'from': 'Capture', 'timestamp': FIXED_TS, 'dlr_token': T_SLOW})
        self.observe('dlr-before-carrier-response', 'db_row_after_dlr', self.message_row(mid))
        self.carrier.slow_release.set()
        th.join(30)
        status, ctype, body = result['r']
        self.cases['send-carrier-response-after-early-dlr'] = {
            'case': 'send-carrier-response-after-early-dlr',
            'note': 'Send completes after a terminal DLR already arrived; legacy overwrites the terminal status with the provider response',
            'request': {'method': 'POST', 'path': '/hermes/v1/sms', 'body': NORM(json.dumps(self.send_body('slow', to=359888000210)))},
            'response': {'status': status, 'content_type': ctype, 'body': body},
            'observed': {'db_row': self.message_row(mid), 'dlr_rows': self.dlr_rows(mid)}}

    def reads(self):
        a = self.bearer(self.tok_a['access_token'])
        b = self.bearer(self.tok_b['access_token'])
        self.call('get-own', 'GET', f'/hermes/v1/sms/{self.m_send}', headers=a)
        self.call('get-own-v1-alias', 'GET', f'/v1/sms/{self.m_send}', headers=a)
        self.call('get-foreign', 'GET', f'/hermes/v1/sms/{self.m_send}', headers=b)
        self.call('get-viewer-foreign', 'GET', f'/hermes/v1/sms/{self.m_send}', headers=self.bearer(self.tok_v['access_token']))
        self.call('get-admin-any', 'GET', f'/hermes/v1/sms/{self.m_send}', headers=self.bearer(self.tok_x['access_token']))
        self.call('get-unknown-authority', 'GET', f'/hermes/v1/sms/{self.m_send}', headers=self.bearer(self.unknown_authority))
        self.call('get-nonexistent', 'GET', '/hermes/v1/sms/00000000-0000-0000-0000-000000000000', headers=a)
        self.call('get-not-a-uuid', 'GET', '/hermes/v1/sms/not-a-uuid', headers=a)
        self.call('get-no-token', 'GET', f'/hermes/v1/sms/{self.m_send}')
        self.call('list-default', 'GET', '/hermes/v1/sms', headers=a)
        self.call('list-v1-alias', 'GET', '/v1/sms', headers=a)
        self.call('list-page-size', 'GET', '/hermes/v1/sms?page=2&pageSize=3', headers=a)
        self.call('list-page-size-snake', 'GET', '/hermes/v1/sms?page=2&page_size=3', headers=a)
        self.call('list-page-beyond', 'GET', '/hermes/v1/sms?page=999&pageSize=50', headers=a)
        self.call('list-negative-paging', 'GET', '/hermes/v1/sms?page=-1&pageSize=-5', headers=a)
        self.call('list-huge-page-size', 'GET', '/hermes/v1/sms?pageSize=100000', headers=a)
        self.call('list-nopaging', 'GET', '/hermes/v1/sms?nopaging=true', headers=a)
        self.call('list-order-by', 'GET', '/hermes/v1/sms?orderBy=-to&orderBy=id', headers=a)
        self.call('list-or-query', 'GET', '/hermes/v1/sms?' + urllib.parse.urlencode({'or': json.dumps({'recipient': '1'})}), headers=a)
        for case, q in (('list-filter-recipient-prefix', {'recipient': '35988800010'}), ('list-filter-status', {'status': 2001}),
                        ('list-filter-status-zero', {'status': 0}), ('list-filter-sid', {'sid': 9999}),
                        ('list-filter-api-client-username-foreign', {'api_client_username': 'client_b'}),
                        ('list-filter-api-client-username-unknown', {'api_client_username': 'nobody'})):
            self.call(case, 'GET', '/hermes/v1/sms?' + urllib.parse.urlencode({'query': json.dumps(q), 'pageSize': 3}), headers=a)
        self.call('list-filter-malformed', 'GET', '/hermes/v1/sms?' + urllib.parse.urlencode({'query': '{not json', 'pageSize': 2}), headers=a)
        self.call('list-filter-sql-text', 'GET', '/hermes/v1/sms?' + urllib.parse.urlencode({'query': json.dumps({'recipient': "1' OR '1'='1"}), 'pageSize': 2}), headers=a)
        self.call('list-viewer-empty', 'GET', '/hermes/v1/sms', headers=self.bearer(self.tok_v['access_token']))
        self.call('list-admin-all', 'GET', '/hermes/v1/sms?pageSize=2', headers=self.bearer(self.tok_x['access_token']))
        self.call('list-admin-filter-username', 'GET', '/hermes/v1/sms?' + urllib.parse.urlencode({'query': json.dumps({'api_client_username': 'client_b'})}),
                  headers=self.bearer(self.tok_x['access_token']))
        self.call('list-unknown-authority', 'GET', '/hermes/v1/sms', headers=self.bearer(self.unknown_authority))
        self.call('list-no-token', 'GET', '/hermes/v1/sms')
        self.call('list-bad-page-type', 'GET', '/hermes/v1/sms?page=abc', headers=a)

    def receipts(self):
        a = self.bearer(self.tok_a['access_token'])
        status, out = self.raw('POST', '/hermes/v1/sms', self.send_body('manual', to=359888000220), a)[:2]
        mid = json.loads(self.raw('GET', '/hermes/v1/sms?' + urllib.parse.urlencode({'query': json.dumps({'recipient': '359888000220'})}), headers=a)[2])['items'][0]['id']
        NORM.name(mid, 'msg:receipts')
        self.m_dlr = mid
        p = {'request_id': mid, 'channel': 'sms', 'sid': 9999, 'to': 359888000220, 'from': 'Capture', 'timestamp': FIXED_TS, 'dlr_token': T_MANUAL}
        self.call('dlrs-none-yet', 'GET', f'/hermes/v1/sms/dlr/{mid}', headers=a)
        self.dlr('dlr-intermediate', {**p, 'message_status': 8})
        self.observe('dlr-intermediate', 'db_row', self.message_row(mid))
        self.dlr('dlr-duplicate-intermediate', {**p, 'message_status': 8, 'timestamp': FIXED_TS + 5})
        self.observe('dlr-duplicate-intermediate', 'dlr_rows', self.dlr_rows(mid))
        self.dlr('dlr-alias-path-terminal', {**p, 'message_status': 1, 'timestamp': FIXED_TS + 10}, path='/hermes/v1/sms/dlr')
        self.observe('dlr-alias-path-terminal', 'db_row', self.message_row(mid))
        self.dlr('dlr-after-terminal', {**p, 'message_status': 2, 'timestamp': FIXED_TS + 20})
        self.observe('dlr-after-terminal', 'db_row', self.message_row(mid))
        self.observe('dlr-after-terminal', 'dlr_rows', self.dlr_rows(mid))
        self.dlr('dlr-wrong-token', {**p, 'message_status': 16, 'dlr_token': 'x' * 32})
        self.dlr('dlr-missing-token', {k: v for k, v in {**p, 'message_status': 16}.items() if k != 'dlr_token'})
        self.dlr('dlr-unknown-request-id', {**p, 'request_id': '00000000-0000-0000-0000-000000000000', 'message_status': 1})
        self.dlr('dlr-empty-request-id', {**p, 'request_id': '', 'message_status': 1})
        self.dlr('dlr-malformed-status', {**p, 'message_status': 'delivered'})
        self.dlr('dlr-unknown-status-code', {**p, 'message_status': 77})
        self.dlr('dlr-post-method', {**p, 'message_status': 16}, method='POST')
        self.dlr('dlr-camel-case-params', {'requestId': mid, 'messageStatus': 16, 'dlr_token': T_MANUAL})
        self.observe('dlr-camel-case-params', 'dlr_rows', self.dlr_rows(mid))
        self.observe('dlr-camel-case-params', 'db_row', self.message_row(mid))
        self.call('dlrs-own', 'GET', f'/hermes/v1/sms/dlr/{mid}', headers=a)
        self.call('dlrs-own-v1-alias', 'GET', f'/v1/sms/dlr/{mid}', headers=a)
        self.call('dlrs-foreign', 'GET', f'/hermes/v1/sms/dlr/{mid}', headers=self.bearer(self.tok_b['access_token']))
        self.call('dlrs-admin', 'GET', f'/hermes/v1/sms/dlr/{mid}', headers=self.bearer(self.tok_x['access_token']))
        self.call('dlrs-nonexistent', 'GET', '/hermes/v1/sms/dlr/00000000-0000-0000-0000-000000000000', headers=a)
        self.call('dlrs-no-token', 'GET', f'/hermes/v1/sms/dlr/{mid}')
        status, out = self.raw('POST', '/hermes/v1/sms', self.send_body('legacy', to=359888000221), a)[:2]
        lid = json.loads(self.raw('GET', '/hermes/v1/sms?' + urllib.parse.urlencode({'query': json.dumps({'recipient': '359888000221'})}), headers=a)[2])['items'][0]['id']
        NORM.name(lid, 'msg:legacy-provider')
        self.dlr('dlr-provider-without-token-accepts-any', {**p, 'request_id': lid, 'to': 359888000221, 'message_status': 1, 'dlr_token': 'anything'})
        self.observe('dlr-provider-without-token-accepts-any', 'db_row', self.message_row(lid))
        # carrier-originated roundtrip through the legacy mock (auto DLR after 300 ms)
        status, out = self.call('send-roundtrip', 'POST', '/hermes/v1/sms', self.send_body('auto', 'hello', to=359888000222, props={'name': 'Eve', 'code': '1'}), a)
        rid = out['data']['id']
        NORM.name(rid, 'msg:roundtrip')
        self.m_round = rid
        for _ in range(50):
            if self.message_row(rid)['status_code'] == 1:
                break
            time.sleep(0.1)
        for row in self.dlr_rows(rid):
            NORM.name(str(row['timestamp']), 'unix_seconds:carrier_dlr')
        self.observe('send-roundtrip', 'db_row_after_carrier_dlr', self.message_row(rid))
        self.observe('send-roundtrip', 'dlr_rows', self.dlr_rows(rid))
        inbox = json.loads(self.raw('GET', '/debug/inbox', port=self.mock_auto)[2])
        entry = next(e for e in inbox if e['decoded']['request_id'] == rid)
        self.observe('send-roundtrip', 'carrier_submission', json.loads(NORM(json.dumps(entry['raw']))))
        self.observe('send-roundtrip', 'carrier_dlr_callbacks', [{'url': NORM(d['url']).replace(str(self.gw_port), '{{gw_port}}'), 'message_status': d['message_status'],
                                                                   'http_status': d['http_status'], 'body': d['body']} for d in entry['dlrs_sent']])
        self.call('dlrs-roundtrip', 'GET', f'/hermes/v1/sms/dlr/{rid}', headers=a)
        self.call('get-roundtrip', 'GET', f'/hermes/v1/sms/{rid}', headers=a)

    def concurrency(self):
        a = self.bearer(self.tok_a['access_token'])
        _, _, body = self.raw('POST', '/hermes/v1/sms', self.send_body('manual', to=359888000230), a)
        mid = json.loads(body)['data']['id']
        NORM.name(mid, 'msg:concurrency')
        q = urllib.parse.urlencode({'request_id': mid, 'channel': 'sms', 'sid': 9999, 'message_status': 8, 'to': 359888000230,
                                    'from': 'Capture', 'timestamp': FIXED_TS, 'dlr_token': T_MANUAL})
        with concurrent.futures.ThreadPoolExecutor(50) as ex:
            statuses = list(ex.map(lambda _: self.raw('GET', '/dlr?' + q)[0], range(100)))
        self.cases['dlr-100-concurrent-same-status'] = {
            'case': 'dlr-100-concurrent-same-status',
            'note': 'Nondeterministic: legacy read-then-insert/increment is not atomic, so concurrent duplicates may be lost or fail on the unique index',
            'observed': {'http_statuses': sorted(set(statuses)), 'dlr_rows': self.dlr_rows(mid), 'db_row': self.message_row(mid)}}

    def webhooks(self):
        deadline = time.time() + 15
        while time.time() < deadline:
            fails = [h for h in self.receiver.hits if h['path'].startswith('/fail')]
            reds = [h for h in self.receiver.hits if h['path'].startswith('/redirect')]
            if len(fails) >= 6 and len(reds) >= 6:
                break
            time.sleep(0.25)
        hits = list(self.receiver.hits)

        def describe(h):
            body = h['body']
            ts = h['headers'].get('X-Smsgw-Timestamp') or h['headers'].get('X-SmsGw-Timestamp')
            sig = h['headers'].get('X-Smsgw-Signature') or h['headers'].get('X-SmsGw-Signature')
            out = {'path': h['path'], 'content_type': h['headers'].get('Content-Type'), 'body': NORM(body.decode()),
                   'header_names': sorted(h['headers']), 'signed': sig is not None}
            if sig:
                expect = hmac.new(CALLBACK_SECRET.encode(), ts.encode() + b'.' + body, hashlib.sha256).hexdigest()
                out.update(signature_valid=hmac.compare_digest(expect, sig), signature_hex_len=len(sig),
                           timestamp_is_unix_seconds=abs(int(ts) - time.time()) < 600)
            return out

        def attempts(prefix):
            group = [h for h in hits if h['path'].startswith(prefix)]
            gaps = [round(b['at'] - a['at'], 1) for a, b in zip(group, group[1:])]
            return {'attempts': len(group), 'gaps_seconds': gaps, 'first': describe(group[0]) if group else None}

        def summary(prefix):
            group = [describe(h) for h in hits if h['path'].startswith(prefix)]
            examples = []
            for d in group:
                if d['body'] not in [e['body'] for e in examples] and len(examples) < 4:
                    examples.append(d)
            return {'deliveries_total': len(group), 'all_signatures_valid': all(d.get('signature_valid', True) for d in group),
                    'all_signed': all(d['signed'] for d in group), 'examples': examples}

        self.cases['webhook-signed'] = {'case': 'webhook-signed', 'note': 'Outbound callbacks for client_a (secret configured); one per accepted receipt, including duplicates',
                                        'observed': summary('/ok/a')}
        self.cases['webhook-unsigned'] = {'case': 'webhook-unsigned', 'note': 'client_b has an empty callback secret',
                                          'observed': summary('/ok/b')}
        self.cases['webhook-retry-on-500'] = {'case': 'webhook-retry-on-500', 'observed': attempts('/fail')}
        self.cases['webhook-redirect-not-followed'] = {'case': 'webhook-redirect-not-followed', 'observed': {**attempts('/redirect'),
                                                       'redirect_target_hits': len([h for h in hits if h['path'] == '/ok'])}}

    def metrics(self):
        status, ctype, body = self.raw('GET', '/metrics')
        families = {}
        for line in body.splitlines():
            if line.startswith('sms_gw_'):
                name, _, rest = line.partition('{')
                labels = sorted({kv.split('=')[0] for kv in rest.split('}')[0].split(',') if '=' in kv}) if rest else []
                base = re.sub(r'_(bucket|sum|count)$', '', name.split(' ')[0])
                families.setdefault(base, set()).update(l for l in labels if l != 'le')
        self.cases['surface-metrics'] = {'case': 'surface-metrics', 'request': {'method': 'GET', 'path': '/metrics'},
                                         'response': {'status': status, 'content_type': ctype, 'body': '<prometheus exposition omitted>'},
                                         'note': 'Scraped after the instance-1 scenarios; unauthenticated on the public listener',
                                         'observed': {'families': {k: sorted(v) for k, v in sorted(families.items())}}}

    def trigger_webhook_clients(self):
        # client_b, client_f and client_r each send one message and receive a DLR on the manual provider
        for tok, to in ((self.tok_b, 359888000240), (self.tok_f, 359888000241), (self.tok_r, 359888000242)):
            _, _, body = self.raw('POST', '/hermes/v1/sms', self.send_body('manual', to=to), self.bearer(tok['access_token']))
            mid = json.loads(body)['data']['id']
            NORM.name(mid, f'msg:webhook-{to}')
            q = urllib.parse.urlencode({'request_id': mid, 'channel': 'sms', 'sid': 9999, 'message_status': 1, 'to': to,
                                        'from': 'Capture', 'timestamp': FIXED_TS, 'dlr_token': T_MANUAL})
            self.raw('GET', '/dlr?' + q)

    def logout(self):
        self.call('logout-no-token', 'POST', '/hermes/v1/logout', {})
        self.call('logout-garbage-token', 'POST', '/hermes/v1/logout', {}, {'Authorization': 'Bearer garbage', 'X-Refresh-Token': 'garbage'})
        tok = self.login('client_l')
        self.tok_l = tok
        now = int(time.time())
        expired = forge_jwt({'authority': 'API_CLIENT', 'kind': 'access', 'username': 'client_l', 'sub': str(self.client_id['client_l']),
                             'iss': 'sms-gw', 'jti': 'e' * 32, 'iat': now - 9000, 'nbf': now - 9000, 'exp': now - 10})
        NORM.name(expired, 'forged_jwt:expired-access-client_l')
        self.call('logout-expired-access-with-refresh', 'POST', '/hermes/v1/logout', {'id': 1},
                  {'Authorization': 'Bearer ' + expired, 'X-Refresh-Token': tok['refresh_token']})
        self.observe('logout-expired-access-with-refresh', 'refresh_after', self.raw('POST', '/hermes/v1/refresh_token', {'refresh_token': tok['refresh_token']})[0])
        self.observe('logout-expired-access-with-refresh', 'access_after_shared_jti', self.raw('GET', '/hermes/v1/me', headers=self.bearer(tok['access_token']))[0])
        tok2 = self.login('client_s')
        self.tok_s = tok2
        self.call('logout-access-and-refresh', 'POST', '/hermes/v1/logout', {},
                  {'Authorization': 'Bearer ' + tok2['access_token'], 'X-Refresh-Token': tok2['refresh_token']})
        self.call('me-after-logout', 'GET', '/hermes/v1/me', headers=self.bearer(tok2['access_token']))
        self.call('refresh-after-logout', 'POST', '/hermes/v1/refresh_token', {'refresh_token': tok2['refresh_token']})
        self.call('logout-get-method', 'GET', '/hermes/v1/logout')

    def restart_with_defaults(self):
        self.stop_gateway()
        self.start_gateway('defaults')
        self.call('restart-revoked-access-accepted', 'GET', '/hermes/v1/me', headers=self.bearer(self.tok_s['access_token']),
                  note='Logout revocations live in process memory; a token revoked before restart is accepted again')
        self.call('restart-old-tokens-still-valid', 'GET', '/hermes/v1/me', headers=self.bearer(self.tok_a['access_token']))
        self.cases['login-rate-limited'] = {'case': 'login-rate-limited', 'note': 'Default SMS_GW_LOGIN_RPM=10 burst 5, keyed by client IP'}
        statuses = []
        for i in range(7):
            status, ctype, body = self.raw('POST', '/hermes/v1/login', {'username': 'client_a', 'password': 'Passw0rd!', 'grant_type': 'password'})
            statuses.append(status)
            if status == 429:
                self.cases['login-rate-limited'].update(request={'method': 'POST', 'path': '/hermes/v1/login'},
                                                        response={'status': status, 'content_type': ctype, 'body': body})
        self.observe('login-rate-limited', 'statuses_in_order', statuses)
        tok = self.tok_a['access_token']
        self.cases['send-rate-limited'] = {'case': 'send-rate-limited', 'note': 'Default SMS_GW_SEND_RPM=100 burst 20, keyed by client id'}
        statuses = []
        for i in range(22):
            status, ctype, body = self.raw('POST', '/hermes/v1/sms', self.send_body('manual', to=359888000250 + i), self.bearer(tok))
            statuses.append(status)
            if status == 429 and 'response' not in self.cases['send-rate-limited']:
                self.cases['send-rate-limited'].update(request={'method': 'POST', 'path': '/hermes/v1/sms'},
                                                       response={'status': status, 'content_type': ctype, 'body': body})
        self.observe('send-rate-limited', 'statuses_in_order', statuses)
        self.observe('send-rate-limited', 'other_client_not_limited',
                     self.raw('POST', '/hermes/v1/sms', self.send_body('manual', to=359888000299), self.bearer(self.tok_b['access_token']))[0])

    # ---------- output ----------

    def write(self):
        for n, row in enumerate(self.sql_json('SELECT id FROM sms_message ORDER BY create_time, id'), 1):
            NORM.name(row['id'], f'msg:seq{n}')
        if OUT.exists():
            shutil.rmtree(OUT)
        OUT.mkdir(parents=True)
        port_re = re.compile(r'(127\.0\.0\.1|198\.51\.100\.1):(%s)' % '|'.join(map(str, (
            self.gw_port, self.mock_auto, self.mock_manual, self.carrier.server_address[1], self.receiver.server_address[1]))))
        secrets = {JWT_SECRET: '{{jwt_secret}}', CALLBACK_SECRET: '{{callback_secret}}', CARRIER_TOKEN: '{{carrier_token}}',
                   T_AUTO: '{{dlr_token:auto}}', T_MANUAL: '{{dlr_token:manual}}', T_SLOW: '{{dlr_token:slow}}'}

        def clean(v):
            if isinstance(v, dict):
                return {k: clean(x) for k, x in v.items()}
            if isinstance(v, list):
                return [clean(x) for x in v]
            if isinstance(v, int) and not isinstance(v, bool) and str(v) in NORM.names:
                return NORM.names[str(v)]
            if isinstance(v, str):
                v = NORM(v)
                for s, name in secrets.items():
                    v = v.replace(s, name)
                return port_re.sub(lambda m: m.group(1) + ':{{port}}', v)
            return v

        for name, rec in self.cases.items():
            (OUT / f'{name}.json').write_text(json.dumps(clean(rec), indent=2, ensure_ascii=False, sort_keys=False) + '\n')
        schema = '\n'.join(l for l in self.schema.splitlines() if l and not l.startswith('--') and not l.startswith('\\'))
        (OUT / 'legacy-schema.sql').write_text(schema + '\n')
        (OUT / 'manifest.json').write_text(json.dumps({
            'source': '../go-tangra-sms-gw', 'source_commit': self.source_rev, 'source_dirty': self.source_dirty,
            'captured_at': time.strftime('%Y-%m-%d'), 'postgres_image': 'postgres:16',
            'carriers': ['legacy cmd/test-linkmobility (auto DLR 300ms and no-DLR instances)', 'in-process fake carrier (reject/http500/drop/slow)'],
            'webhook_receiver': 'in-process receiver on the TEST-NET-2 bridge address',
            'placeholders': 'Values in {{...}} replace generated UUIDs, JWTs, RFC 3339 times, ports and capture-only secrets',
            'cases': sorted(self.cases)}, indent=2) + '\n')
        print(f'captured {len(self.cases)} runtime cases into {OUT.relative_to(ROOT)}')


def main():
    # --snapshot PATH: run the same scenario but, instead of rewriting the
    # fixtures, pg_dump the populated legacy database to PATH (a throwaway
    # legacy snapshot for the migration rehearsal; it holds capture-only
    # credentials, keep it outside the repository).
    snapshot = None
    if len(sys.argv) == 3 and sys.argv[1] == '--snapshot':
        snapshot = Path(sys.argv[2]).resolve()
    elif len(sys.argv) != 1:
        sys.exit('usage: capture_legacy_runtime.py [--snapshot PATH]')
    with tempfile.TemporaryDirectory(prefix='smsgw-legacy-runtime-') as temp:
        cap = Capture(Path(temp))
        try:
            cap.build()
            cap.certs()
            cap.database()
            cap.start_services()
            cap.start_gateway('relaxed')
            cap.seed()
            cap.scenarios()
            cap.trigger_webhook_clients()
            cap.webhooks()
            cap.metrics()
            cap.restart_with_defaults()
            if snapshot:
                cap.stop_gateway()
                snapshot.write_text(run(['docker', 'exec', PG, 'pg_dump', '-U', 'postgres', '--no-owner', '--no-privileges', 'sms_gw']))
                print(f'legacy snapshot written to {snapshot}')
            else:
                cap.write()
        except Exception:
            for log in Path(temp).glob('gateway-*.log'):
                sys.stderr.write(log.read_text()[-4000:])
            raise
        finally:
            cap.teardown()


if __name__ == '__main__':
    main()
