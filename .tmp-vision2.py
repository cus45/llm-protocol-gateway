import base64
import json
import sqlite3
import struct
import urllib.error
import urllib.request
import zlib

DB = '/var/lib/llm-protocol-gateway/gateway.db'
URL = 'http://127.0.0.1:18093/anthropic/v1/messages'
c = sqlite3.connect(DB, isolation_level=None)
KEY, ROUTE, MODEL_OVERRIDE = c.execute(
    "select key, route_id, model_override from api_keys where id='work-buddy'").fetchone()
MODEL = MODEL_OVERRIDE or 'deepseek-v4.1-flash'


def solid_png(r, g, b, size=32):
    def chunk(tag, data):
        return struct.pack('>I', len(data)) + tag + data + struct.pack('>I', zlib.crc32(tag + data) & 0xFFFFFFFF)
    raw = b''.join(b'\x00' + bytes([r, g, b]) * size for _ in range(size))
    return (b'\x89PNG\r\n\x1a\n'
            + chunk(b'IHDR', struct.pack('>IIBBBBB', size, size, 8, 2, 0, 0, 0))
            + chunk(b'IDAT', zlib.compress(raw, 9)) + chunk(b'IEND', b''))


def post(body):
    req = urllib.request.Request(URL, data=json.dumps(body).encode(),
                                 headers={'Authorization': 'Bearer ' + KEY, 'Content-Type': 'application/json'},
                                 method='POST')
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            obj = json.loads(r.read().decode('utf-8', 'replace'))
            return 200, ' '.join(b.get('text', '') for b in (obj.get('content') or []) if b.get('type') == 'text')
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode('utf-8', 'replace')[:300]


png = base64.b64encode(solid_png(220, 20, 20)).decode()
print('route =', ROUTE, '| model =', MODEL)

cases = {
    'B 用户消息带图': [
        {"role": "user", "content": [
            {"type": "text", "text": "这张图是什么颜色？只答颜色名，两个字。"},
            {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": png}},
        ]},
    ],
    'C2 工具结果带图（追问单独一条 user）': [
        {"role": "user", "content": "截图给我看看页面。"},
        {"role": "assistant", "content": [
            {"type": "tool_use", "id": "toolu_snap_1", "name": "browser_take_snapshot", "input": {}}]},
        {"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": "toolu_snap_1", "content": [
                {"type": "text", "text": "snapshot taken"},
                {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": png}},
            ]}]},
        {"role": "user", "content": "刚才工具返回的截图是什么颜色？只答颜色名，两个字。"},
    ],
    'C 工具结果带图（追问同一条 user）': [
        {"role": "user", "content": "截图给我看看页面。"},
        {"role": "assistant", "content": [
            {"type": "tool_use", "id": "toolu_snap_1", "name": "browser_take_snapshot", "input": {}}]},
        {"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": "toolu_snap_1", "content": [
                {"type": "text", "text": "snapshot taken"},
                {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": png}},
            ]},
            {"type": "text", "text": "刚才工具返回的截图是什么颜色？只答颜色名，两个字。"},
        ]},
    ],
}

TOOLS = [{"name": "browser_take_snapshot", "description": "snapshot",
          "input_schema": {"type": "object", "properties": {}}}]

for round in range(3):
    for name, messages in cases.items():
        body = {"model": MODEL, "max_tokens": 64, "messages": messages}
        if name.startswith('C'):
            body["tools"] = TOOLS
        code, text = post(body)
        print('%d %-32s -> %s %s' % (round, name, code, text[:80].replace('\n', ' ')))
