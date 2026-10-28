#!/usr/bin/env node
// 用 Node 内置的 zlib 打 zip：Windows 的 Git Bash 不带 zip 命令，
// 而 CI 与本地发版共用同一份脚本，不能一边有 zip 一边没有。
//
//   node zip-dir.mjs 输出.zip 目录
import {mkdirSync, readdirSync, readFileSync, statSync, writeFileSync} from 'node:fs';
import {dirname, join, relative, sep} from 'node:path';
import {crc32, deflateRawSync} from 'node:zlib';

const [outPath, dir] = process.argv.slice(2);
if (!outPath || !dir) {
  console.error('usage: node zip-dir.mjs <out.zip> <dir>');
  process.exit(2);
}

function walk(base, current) {
  const entries = [];
  for (const name of readdirSync(current).sort()) {
    const full = join(current, name);
    const info = statSync(full);
    if (info.isDirectory()) {
      entries.push({ name: `${relative(base, full).split(sep).join('/')}/`, dir: true });
      entries.push(...walk(base, full));
    } else {
      entries.push({ name: relative(base, full).split(sep).join('/'), file: full, info });
    }
  }
  return entries;
}

function dosTime(date) {
  const time =
    (date.getHours() << 11) | (date.getMinutes() << 5) | Math.floor(date.getSeconds() / 2);
  const day = date.getDate();
  const month = date.getMonth() + 1;
  const year = Math.max(date.getFullYear() - 1980, 0);
  return { time, date: (year << 9) | (month << 5) | day };
}

const local = [];
const central = [];
let offset = 0;

for (const entry of walk(dir, dir)) {
  const nameBuf = Buffer.from(entry.name, 'utf8');
  const { time, date } = dosTime(entry.info?.mtime ?? new Date());
  const data = entry.dir ? Buffer.alloc(0) : readFileSync(entry.file);
  const compressed = entry.dir ? data : deflateRawSync(data, { level: 9 });
  const crc = entry.dir ? 0 : crc32(data);

  const header = Buffer.alloc(30);
  header.writeUInt32LE(0x04034b50, 0);
  header.writeUInt16LE(20, 4);
  header.writeUInt16LE(0x0800, 6); // 文件名按 UTF-8
  header.writeUInt16LE(8, 8);
  header.writeUInt16LE(time, 10);
  header.writeUInt16LE(date, 12);
  header.writeUInt32LE(crc, 14);
  header.writeUInt32LE(compressed.length, 18);
  header.writeUInt32LE(data.length, 22);
  header.writeUInt16LE(nameBuf.length, 26);
  header.writeUInt16LE(0, 28);

  local.push(header, nameBuf, compressed);

  const cd = Buffer.alloc(46);
  cd.writeUInt32LE(0x02014b50, 0);
  cd.writeUInt16LE(0x031e, 4); // made by Unix, so the mode below is honoured
  cd.writeUInt16LE(20, 6);
  cd.writeUInt16LE(0x0800, 8);
  cd.writeUInt16LE(8, 10);
  cd.writeUInt16LE(time, 12);
  cd.writeUInt16LE(date, 14);
  cd.writeUInt32LE(crc, 16);
  cd.writeUInt32LE(compressed.length, 20);
  cd.writeUInt32LE(data.length, 24);
  cd.writeUInt16LE(nameBuf.length, 28);
  // 压缩方式 0（store，目录条目）或 8（deflate）
  cd.writeUInt16LE(entry.dir ? 0 : 8, 10);
  cd.writeUInt32LE(((entry.dir || entry.name.endsWith('.exe') ? 0o100755 : 0o100644) << 16) >>> 0, 38);
  cd.writeUInt32LE(offset, 42);
  central.push(cd, nameBuf);

  offset += header.length + nameBuf.length + compressed.length;
}

const centralBuf = Buffer.concat(central);
const end = Buffer.alloc(22);
end.writeUInt32LE(0x06054b50, 0);
end.writeUInt16LE(local.length / 3, 8);
end.writeUInt16LE(local.length / 3, 10);
end.writeUInt32LE(centralBuf.length, 12);
end.writeUInt32LE(offset, 16);

mkdirSync(dirname(outPath), { recursive: true });
writeFileSync(outPath, Buffer.concat([...local, centralBuf, end]));
console.log(`${outPath} (${local.length / 3} entries)`);
