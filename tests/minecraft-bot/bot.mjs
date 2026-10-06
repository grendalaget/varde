// varde-minecraft-bot: writes/reads an 8-hex-digit nonce encoded as a
// column of 8 wool blocks in a creative-mode Minecraft server.
//
//   node bot.mjs write --host H --port 25565 --version 1.21.4 \
//     --user varde_tester --col N --nonce HEX8 [--ref x,y,z]
//   node bot.mjs read  --host H --port 25565 --version 1.21.4 \
//     --user varde_tester --col N --ref x,y,z
//
// One JSON line on stdout, exit 0 on success, non-zero on failure.

import mineflayer from 'mineflayer'
import { Vec3 } from 'vec3'
import prismarineItem from 'prismarine-item'

const WOOL = [
  'white', 'orange', 'magenta', 'light_blue', 'yellow', 'lime', 'pink',
  'gray', 'light_gray', 'cyan', 'purple', 'blue', 'brown', 'green', 'red',
  'black',
]

function parseArgs(argv) {
  const out = { _: argv[2] }
  for (let i = 3; i < argv.length; i += 2) out[argv[i].replace(/^--/, '')] = argv[i + 1]
  return out
}

function die(msg, code = 1) {
  console.error(msg)
  process.exit(code)
}

const args = parseArgs(process.argv)
const mode = args._
if (mode !== 'write' && mode !== 'read') die('usage: bot.mjs write|read --host H --port P --version V --user U --col N [--nonce HEX8] [--ref x,y,z]')
const host = args.host, port = Number(args.port ?? 25565)
const col = Number(args.col ?? 0)
let nonce = (args.nonce ?? '').toLowerCase()
if (mode === 'write' && !/^[0-9a-f]{8}$/.test(nonce)) die('--nonce must be 8 hex digits')

const bot = mineflayer.createBot({
  host, port,
  username: args.user ?? 'varde_tester',
  version: args.version ?? '1.21.4',
  auth: 'offline',
})

const hardTimeout = setTimeout(() => {
  console.error('bot: hard timeout')
  try { bot.quit() } catch {}
  process.exit(1)
}, 60_000)

let errored = false
bot.once('error', (e) => { errored = true; console.error('bot error:', e.message ?? e); process.exitCode = 1 })
bot.once('kicked', (r) => { console.error('kicked:', r); process.exitCode = 1 })

function parseRef(s, bot) {
  if (s) {
    const [x, y, z] = s.split(',').map(Number)
    if ([x, y, z].some(Number.isNaN)) die('--ref must be x,y,z')
    return new Vec3(x, y, z)
  }
  // reference ground block = first solid block at/below the spawn point
  const p = bot.entity.position.floored()
  for (let y = p.y - 1; y > p.y - 30; y--) {
    const b = bot.blockAt(new Vec3(p.x, y, p.z))
    if (b && b.name !== 'air' && b.name !== 'cave_air' && b.boundingBox === 'block') return new Vec3(p.x, y, p.z)
  }
  die('bot: no ground block found under spawn')
}

const colX = (ref, spawnX) => (args.ref ? ref.x : spawnX) + 2 + 2 * col

// columnBase finds the top solid block at (x, z) near the anchor height —
// terrain under the column can sit a few blocks above or below the ground
// under the spawn block.
function columnBase(x, z, anchorY) {
  for (let y = anchorY + 16; y > anchorY - 16; y--) {
    const b = bot.blockAt(new Vec3(x, y, z))
    if (b && b.name !== 'air' && b.name !== 'cave_air' && b.boundingBox === 'block') return b.position
  }
  return null
}

// stage wraps a step with a named timeout so a hang says where it hung
// instead of the opaque 60 s hard timeout.
function stage(name, p, ms = 20_000) {
  return Promise.race([
    p,
    new Promise((_, rej) => setTimeout(() => rej(new Error(`${name} timeout`)), ms)),
  ])
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// goTo walks the bot toward (x, z) — jumping over 1-block bumps — until it
// stands on the column.
async function goTo(x, z) {
  const cx = x + 0.5, cz = z + 0.5
  for (let i = 0; i < 300; i++) {
    const p = bot.entity.position
    if (Math.hypot(p.x - cx, p.z - cz) < 0.35) break
    await bot.lookAt(new Vec3(cx, p.y, cz))
    bot.setControlState('forward', true)
    if (i % 20 === 19) {
      bot.setControlState('jump', true)
      setTimeout(() => bot.setControlState('jump', false), 250)
    }
    await sleep(50)
  }
  bot.setControlState('forward', false)
}

// pillarOnce jumps and places the held block directly beneath the bot;
// resolves after the placement lands.
async function waitLanded() {
  for (let i = 0; i < 40; i++) {
    if (bot.entity.onGround) return
    await sleep(50)
  }
  throw new Error('never landed on ground')
}

async function pillarOnce(x, z) {
  // the server can refuse a block placed at the apex if its view of the
  // player's bounding box still overlaps the target cell — retry the whole
  // jump+place until the block lands
  let lastErr
  for (let attempt = 0; attempt < 5; attempt++) {
    await waitLanded()
    // make sure we stand on the column before referencing the block below
    await goTo(x, z)
    // reference = the block under our feet BEFORE jumping (at jump apex the
    // position below is already the new air gap)
    const below = bot.blockAt(bot.entity.position.offset(0, -0.5, 0))
    if (!below || below.name === 'air' || below.name === 'cave_air') {
      lastErr = new Error('no block under feet while pillaring')
      continue
    }
    const jumpY = Math.floor(bot.entity.position.y) + 1.0
    bot.setControlState('jump', true)
    for (let i = 0; i < 40; i++) {
      if (bot.entity.position.y > jumpY) break
      await sleep(50)
    }
    bot.setControlState('jump', false)
    if (bot.entity.position.y <= jumpY) {
      // wedged (e.g. spawned inside a block, or a leaf/ceiling overhead):
      // dig the blocks at head/ceiling level, step back, retry fresh
      lastErr = new Error(`jump did not rise (y=${bot.entity.position.y.toFixed(2)} want >${jumpY})`)
      for (const dy of [1, 2]) {
        const b = bot.blockAt(bot.entity.position.offset(0, dy, 0))
        if (b && b.name !== 'air' && b.name !== 'cave_air' && b.diggable) {
          await bot.dig(b).catch(() => {})
        }
      }
      bot.setControlState('back', true)
      await sleep(400)
      bot.setControlState('back', false)
      continue
    }
    try {
      await bot.placeBlock(below, new Vec3(0, 1, 0))
      return
    } catch (e) {
      lastErr = e
      await sleep(100)
    }
  }
  throw lastErr
}

async function main() {
  await new Promise((res, rej) => {
    bot.once('spawn', res)
    setTimeout(() => rej(new Error('spawn timeout')), 45_000)
  }).catch((e) => die('bot: ' + e.message))
  await stage('waitForChunksToLoad', bot.waitForChunksToLoad(), 30_000).catch((e) => die('bot: ' + e.message))

  const spawn = bot.entity.position.floored()
  const ref = parseRef(args.ref, bot)
  const x = colX(ref, spawn.x)
  const z = args.ref ? ref.z : spawn.z

  if (mode === 'write') {
    const base = columnBase(x, z, ref.y)
    if (!base) die(`bot: no ground under column at ${x},${z}`)
    await stage('walk to column', goTo(x, z), 15_000).catch((e) => die('bot: ' + e.message))
    const Item = prismarineItem(bot.version)
    const digits = nonce.split('').map((c) => parseInt(c, 16))
    for (let i = 0; i < 8; i++) {
      const item = new Item(bot.registry.itemsByName[WOOL[digits[i]] + '_wool'].id, 1)
      await stage('setInventorySlot', bot.creative.setInventorySlot(36, item)).catch((e) => die('bot: ' + e.message))
      bot.setQuickBarSlot(0)
      // pillar: place the wool under ourselves while jumping — the bot
      // lands on it and the column grows one block per iteration. No
      // flight needed (allow-flight is off by default).
      await stage(`pillar ${i}`, pillarOnce(x, z)).catch((e) => die('bot: ' + e.message))
    }
    console.log(JSON.stringify({ ok: true, ref: `${ref.x},${ref.y},${ref.z}` }))
  } else {
    // find the contiguous 8-wool run near the anchor height (the column
    // base is the column-local ground, which can differ from the anchor)
    const names = []
    for (let y = ref.y - 15; y <= ref.y + 24; y++) {
      const b = bot.blockAt(new Vec3(x, y, z))
      names.push(b ? b.name : 'unloaded')
    }
    let nonce = ''
    for (let i = 0; i + 8 <= names.length; i++) {
      if (names.slice(i, i + 8).every((n) => n.endsWith('_wool'))) {
        nonce = names.slice(i, i + 8).map((n) => WOOL.indexOf(n.slice(0, -5)).toString(16)).join('')
        break
      }
    }
    if (nonce === '') {
      const expected = names.slice(16, 24)
      console.log(JSON.stringify({ nonce: null, blocks: expected }))
    } else {
      console.log(JSON.stringify({ nonce }))
    }
  }
  clearTimeout(hardTimeout)
  bot.quit()
  setTimeout(() => process.exit(errored ? 1 : 0), 300)
}

main().catch((e) => {
  console.error('bot:', e.message ?? e)
  clearTimeout(hardTimeout)
  try { bot.quit() } catch {}
  process.exit(1)
})
