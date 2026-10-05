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

async function main() {
  await new Promise((res, rej) => {
    bot.once('spawn', res)
    setTimeout(() => rej(new Error('spawn timeout')), 45_000)
  }).catch((e) => die('bot: ' + e.message))
  await bot.waitForChunksToLoad()

  const spawn = bot.entity.position.floored()
  const ref = parseRef(args.ref, bot)
  const x = colX(ref, spawn.x)
  const z = args.ref ? ref.z : spawn.z

  if (mode === 'write') {
    const Item = prismarineItem(bot.version)
    const digits = nonce.split('').map((c) => parseInt(c, 16))
    for (let i = 0; i < 8; i++) {
      const target = new Vec3(x, ref.y + 1 + i, z)
      // fly adjacent to the target so it is within place reach
      await bot.creative.flyTo(new Vec3(x + 1, target.y + 1, z))
      const item = new Item(bot.registry.itemsByName[WOOL[digits[i]] + '_wool'].id, 1)
      await bot.creative.setInventorySlot(36, item)
      bot.setQuickBarSlot(0)
      const below = bot.blockAt(new Vec3(x, target.y - 1, z))
      if (!below || below.name === 'air') die(`bot: missing block under column at ${x},${target.y - 1},${z}`)
      await bot.placeBlock(below, new Vec3(0, 1, 0))
    }
    console.log(JSON.stringify({ ok: true, ref: `${ref.x},${ref.y},${ref.z}` }))
  } else {
    const names = []
    let nonce = ''
    for (let i = 0; i < 8; i++) {
      const b = bot.blockAt(new Vec3(x, ref.y + 1 + i, z))
      const name = b ? b.name : 'unloaded'
      names.push(name)
      const wool = name.endsWith('_wool') ? name.slice(0, -5) : null
      const digit = wool ? WOOL.indexOf(wool) : -1
      nonce += digit >= 0 ? digit.toString(16) : '?'
    }
    if (nonce.includes('?')) {
      console.log(JSON.stringify({ nonce: null, blocks: names }))
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
