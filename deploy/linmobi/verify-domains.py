import argparse
import http.client
import json
import os
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


parser = argparse.ArgumentParser()
parser.add_argument('--panel-cdn', action='store_true',
                    help='Expect Cloudflare Always Use HTTPS redirects on the panel host')
args = parser.parse_args()
opener = urllib.request.build_opener(NoRedirect)
opener.addheaders = [('User-Agent', 'Sub2API-Deployment-Check/1.0')]
results = []


def check(host, path, status, method='GET', scheme='https', location=None):
    url = f'{scheme}://{host}{path}'
    req = urllib.request.Request(url, method=method)
    if method == 'POST':
        req.data = b'{}'
        req.add_header('Content-Type', 'application/json')
    try:
        response = opener.open(req, timeout=15)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        body = response.read()
        ok = response.code == status
        # Nginx's standard HTTP-to-HTTPS redirect has a small HTML body.
        # It must point to the same API hostname, and never serves the panel.
        if (host == 'api.ai.lin.mobi' and status != 308) or status == 404:
            ok = ok and b'<html' not in body.lower() and b'<!doctype' not in body.lower()
        if location:
            ok = ok and response.headers.get('Location') == location
        results.append({'url': url, 'method': method, 'status': response.code,
                        'bytes': len(body), 'type': response.headers.get('Content-Type'),
                        'pass': ok})


panel = 'ai.lin.mobi'
api = 'api.ai.lin.mobi'
for path in ['/', '/login', '/api/v1/settings/public', '/setup/status']:
    check(panel, path, 200)
gateway_paths = [
    '/v1/models', '/v1beta/models', '/antigravity/v1/models',
    '/backend-api/codex/models', '/models', '/models/test', '/responses',
    '/chat/completions', '/messages/count_tokens', '/embeddings',
    '/alpha/search', '/tts', '/stt', '/realtime', '/custom-voices',
    '/images/generations', '/videos', '/web_search', '/x_search',
    '/api/v3/contents/generations/tasks', '/v3/contents/generations/tasks',
    '/contents/generations/tasks', '/api/event_logging/batch',
]
for path in gateway_paths:
    check(panel, path, 404)
for path in ['/v1/models', '/v1beta/models', '/antigravity/v1/models',
             '/backend-api/codex/models', '/models', '/models/test']:
    check(api, path, 401)
for path in ['/v1/chat/completions', '/v1/messages', '/v1/responses',
             '/chat/completions', '/embeddings', '/alpha/search',
             '/custom-voices', '/tts', '/stt', '/videos',
             '/api/v3/contents/generations/tasks', '/v3/contents/generations/tasks',
             '/contents/generations/tasks']:
    check(api, path, 401, method='POST')
for path in ['/', '/login', '/admin', '/assets/index.js', '/index.html',
             '/api/v1/auth/login', '/api/v1/auth/me', '/api/v1/admin/settings',
             '/api/v1/settings/public', '/setup/status', '/v1', '/v1/',
             '/v1/does-not-exist', '/v1/../login', '/v1/%2e%2e/login',
             '/alpha/search/unknown', '/models/../login']:
    check(api, path, 404)
check(api, '/api/v1/auth/login', 404, method='POST')
check(panel, '/v1%2fmodels', 404)
check(panel, '//v1//models', 404)
check(panel, '/login', 301 if args.panel_cdn else 308, scheme='http', location='https://ai.lin.mobi/login')
check(api, '/v1/models', 308, scheme='http', location='https://api.ai.lin.mobi/v1/models')
# Cloudflare redirects HTTP before origin routing; HTTPS remains blocked above.
check(panel, '/v1/models', 301 if args.panel_cdn else 404, scheme='http',
      location='https://ai.lin.mobi/v1/models' if args.panel_cdn else None)
check(api, '/login', 404, scheme='http')

for scheme in ['http', 'https']:
    for path in ['/', '/login', '/v1/models', '/api/v1/auth/me']:
        url = f'{scheme}://probe.ai.lin.mobi{path}'
        try:
            with opener.open(url, timeout=15) as response:
                response.read()
            ok = False
        except http.client.RemoteDisconnected:
            ok = True
        except urllib.error.URLError as error:
            ok = any(word in str(error.reason).lower() for word in ['closed', 'reset'])
        results.append({'url': url, 'empty_connection_close': ok, 'pass': ok})

report = {'checked_at': datetime.now(timezone.utc).isoformat(),
          'checks': len(results), 'passed': all(row['pass'] for row in results),
          'results': results}
state = Path('/var/lib/sub2api-proxy/domain-separation-verification.json')
state.write_text(json.dumps(report, indent=2) + '\n')
os.chmod(state, 0o600)
print(json.dumps({'checks': len(results), 'passed': report['passed'],
                  'failures': [row for row in results if not row['pass']],
                  'report': str(state)}))
if not report['passed']:
    raise SystemExit(1)
