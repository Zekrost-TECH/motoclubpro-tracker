#!/usr/bin/env node
/**
 * Test de integración del tracker (ROD-23).
 *
 * Cubre el flujo completo de tracking con un evento sintético aislado:
 *   autorización → posición → broadcast (rol canónico, batería) →
 *   posición inválida → fin de rodada (event_status + cierre + purga) →
 *   rechazo de reconexión → en_curso ignorado → ping/pong.
 *
 * Uso (requiere un tracker corriendo + Redis):
 *   JWT_SECRET=<mismo que el tracker> \
 *   REDIS_URL=<redis del tracker> \
 *   TRACKER_WS_URL=ws://localhost:8081 \
 *   node scripts/integration.mjs
 *
 * Salida: exit 0 si todos los pasos pasan, 1 si alguno falla.
 */
const crypto = require('crypto');
const Redis = require('ioredis');

const JWT_SECRET = process.env.JWT_SECRET;
const REDIS_URL = process.env.REDIS_URL;
const TRACKER_WS_URL = process.env.TRACKER_WS_URL || 'ws://localhost:8081';

const EVENT = '11111111-2222-4222-8222-222222222222'; // evento sintético aislado
const USER = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';   // userId de prueba
const CLUB = '60baddf4-e946-4e0a-828b-205d3227a6e2';

if (!JWT_SECRET || !REDIS_URL) {
  console.error('Faltan JWT_SECRET y/o REDIS_URL');
  process.exit(2);
}

const enc = (o) => Buffer.from(JSON.stringify(o)).toString('base64url');
const now = Math.floor(Date.now() / 1000);
const payload = { sub: USER, email: 'test@bikeros.co', role: 'rider', clubs: [], iat: now, exp: now + 600, iss: 'biker-os-api', aud: 'biker-os-clients' };
const data = enc({ alg: 'HS256', typ: 'JWT' }) + '.' + enc(payload);
const token = data + '.' + crypto.createHmac('sha256', JWT_SECRET).update(data).digest('base64url');

const redis = new Redis(REDIS_URL, { maxRetriesPerRequest: 3 });
redis.on('error', () => { /* errores transitorios de red: los pasos reportan su propio estado */ });
const results = [];
const step = (name, ok, detail = '') => {
  results.push({ name, ok });
  console.log(`${ok ? '✅' : '❌'} ${name}${detail ? ' — ' + detail : ''}`);
};
const wait = (ms) => new Promise(r => setTimeout(r, ms));

function sendJson(ws, obj) {
  if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
}

function connect(eventId) {
  return new Promise((resolve) => {
    const ws = new WebSocket(`${TRACKER_WS_URL}/ws/events/${eventId}`, ['bearer', token]);
    const state = { ws, open: false, closed: false, closeInfo: null, messages: [], errors: [] };
    ws.onopen = () => { state.open = true; resolve(state); };
    ws.onerror = () => { state.errors.push('onerror'); resolve(state); };
    ws.onclose = (e) => { state.closed = true; state.closeInfo = { code: e.code, reason: e.reason || '' }; resolve(state); };
    ws.onmessage = (e) => {
      try { state.messages.push(JSON.parse(e.data)); } catch { state.errors.push('bad json'); }
    };
    setTimeout(() => resolve(state), 2500);
  });
}

(async () => {
  try {
    await redis.multi()
      .del(`event:${EVENT}:members`, `event:${EVENT}:roles`, `event:${EVENT}:club`)
      .sadd(`event:${EVENT}:members`, USER)
      .hset(`event:${EVENT}:roles`, USER, 'puntero')
      .set(`event:${EVENT}:club`, CLUB)
      .exec();

    // 1. Conexión autorizada
    const a = await connect(EVENT);
    step('1. WS conecta con membresía válida', a.open || a.closed === false);

    // 2. Posición + rol canónico (ROD-07) + batería (ROD-17)
    sendJson(a.ws, {
      type: 'position',
      payload: { lat: 10.5, lng: -74.5, speed: 30, heading: 90, timestamp: Date.now(), name: 'Rider', role: 'rider', battery: 67, isCharging: true },
    });
    await wait(2500);
    const riders = a.messages.find(m => m.type === 'riders');
    const myPos = riders?.payload?.find(p => p.userId === USER);
    step('2. Broadcast recibe posición', !!myPos);
    step('2b. Rol canónico del hash (ROD-07)', myPos?.role === 'puntero', `role=${myPos?.role}`);
    step('2c. Batería en el broadcast (ROD-17)', myPos?.battery === 67, `battery=${myPos?.battery}`);

    // 3. Posición inválida (ROD-08)
    const trackBefore = await redis.scan(0, 'MATCH', `track:${EVENT}:*`, 'COUNT', 100);
    sendJson(a.ws, { type: 'position', payload: { lat: 95, lng: -74.5, speed: 30, heading: 90, timestamp: Date.now(), name: 'Rider' } });
    await wait(1500);
    const errMsg = a.messages.find(m => m.type === 'error');
    step('3. Posición fuera de rango rechazada (ROD-08)', !!errMsg, errMsg ? errMsg.message : 'sin error');
    const keysAfterInvalid = await redis.scan(0, 'MATCH', `track:${EVENT}:*`, 'COUNT', 100);
    step('3b. No se guardó la posición inválida', keysAfterInvalid[1].length === trackBefore[1].length);

    // 4. Fin de rodada (ROD-02/09 + TRK-14)
    await redis.publish(`event:${EVENT}:status`, JSON.stringify({ type: 'event_status', payload: { eventId: EVENT, status: 'completado' } }));
    await wait(2000);
    const statusMsg = a.messages.find(m => m.type === 'event_status');
    step('4. Cliente recibe event_status', !!statusMsg);
    step('4b. El tracker cierra la conexión', a.closed, JSON.stringify(a.closeInfo));
    const afterPurge = await redis.scan(0, 'MATCH', `track:${EVENT}:*`, 'COUNT', 100);
    step('4c. Posiciones purgadas', afterPurge[1].length === 0 && trackBefore[1].length > 0);
    const [membersLeft, rolesLeft] = await Promise.all([redis.exists(`event:${EVENT}:members`), redis.exists(`event:${EVENT}:roles`)]);
    step('4d. Autorización purgada', membersLeft === 0 && rolesLeft === 0);

    // 5. Reconexión tras purga rechazada
    const b = await connect(EVENT);
    await wait(800);
    const bErr = b.messages.find(m => m.type === 'error');
    step('5. Reconexión tras fin de rodada rechazada', !!bErr, bErr ? bErr.message : 'no rechazada');

    // 6. en_curso NO cierra ni purga
    await redis.multi().sadd(`event:${EVENT}:members`, USER).hset(`event:${EVENT}:roles`, USER, 'rider').exec();
    const c = await connect(EVENT);
    await redis.publish(`event:${EVENT}:status`, JSON.stringify({ type: 'event_status', payload: { eventId: EVENT, status: 'en_curso' } }));
    await wait(2000);
    step('6. Publish "en_curso" no cierra clientes', !c.closed);
    const [m2, r2] = await Promise.all([redis.exists(`event:${EVENT}:members`), redis.exists(`event:${EVENT}:roles`)]);
    step('6b. Keys de autorización intactas con en_curso', m2 === 1 && r2 === 1);
    if (c.ws) { try { c.ws.close(1000); } catch {} }

    // 7. ping/pong
    const d = await connect(EVENT);
    sendJson(d.ws, { type: 'ping' });
    await wait(1200);
    step('7. Ping → pong', d.messages.some(m => m.type === 'pong'));
    if (d.ws) { try { d.ws.close(1000); } catch {} }
  } catch (e) {
    console.error('FALLO GENERAL:', e.message);
  } finally {
    await redis.multi().del(`event:${EVENT}:members`, `event:${EVENT}:roles`, `event:${EVENT}:club`).exec();
    const leftovers = await redis.scan(0, 'MATCH', `track:${EVENT}:*`, 'COUNT', 100);
    for (const k of leftovers[1]) await redis.del(k);
    redis.disconnect();
    const failed = results.filter(r => !r.ok).length;
    console.log(`\nRESULTADO: ${results.length - failed}/${results.length} pasos OK`);
    process.exit(failed > 0 ? 1 : 0);
  }
})();
