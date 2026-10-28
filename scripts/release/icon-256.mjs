#!/usr/bin/env node
// 把仓库里的 512×512 应用图标缩成 go-winres 能接受的 ico 尺寸来源（≤256）。
// 仓库根目录没有 node_modules，所以这里手写 PNG 解码/编码（8 位 RGB/RGBA），
// 不引入外部依赖；Windows 安装包与桌面 App 共用同一张图标。
//
//   node icon-256.mjs <输入.png> <输出.png>
import {readFileSync, writeFileSync} from 'node:fs';
import {crc32, deflateSync, inflateSync} from 'node:zlib';

const [src, out] = process.argv.slice(2);
if (!src || !out) {
  console.error('usage: node icon-256.mjs <in.png> <out.png>');
  process.exit(2);
}

const SIZE = 256;

function decodePng(buf) {
  let pos = 8;
  let width = 0;
  let height = 0;
  let bitDepth = 0;
  let colorType = 0;
  let interlace = 0;
  const idat = [];
  while (pos < buf.length) {
    const len = buf.readUInt32BE(pos);
    const type = buf.toString('ascii', pos + 4, pos + 8);
    const data = buf.subarray(pos + 8, pos + 8 + len);
    if (type === 'IHDR') {
      width = data.readUInt32BE(0);
      height = data.readUInt32BE(4);
      bitDepth = data[8];
      colorType = data[9];
      interlace = data[12];
    } else if (type === 'IDAT') {
      idat.push(data);
    } else if (type === 'IEND') {
      break;
    }
    pos += 12 + len;
  }
  if (bitDepth !== 8 || (colorType !== 6 && colorType !== 2) || interlace !== 0) {
    throw new Error(`不支持的 PNG 格式（bitDepth=${bitDepth} colorType=${colorType} interlace=${interlace}）`);
  }
  const channels = colorType === 6 ? 4 : 3;
  const raw = new Uint8Array(inflateSync(Buffer.concat(idat)));
  const stride = width * channels;
  const pixels = new Uint8Array(width * height * 4);
  let prev = new Uint8Array(stride);
  for (let y = 0; y < height; y++) {
    const filter = raw[y * (stride + 1)];
    const line = raw.subarray(y * (stride + 1) + 1, (y + 1) * (stride + 1));
    const cur = new Uint8Array(stride);
    for (let i = 0; i < stride; i++) {
      const a = i >= channels ? cur[i - channels] : 0;
      const b = prev[i];
      const c = i >= channels ? prev[i - channels] : 0;
      let value = line[i];
      switch (filter) {
        case 1: value += a; break;
        case 2: value += b; break;
        case 3: value += (a + b) >> 1; break;
        case 4: {
          const p = a + b - c;
          const pa = Math.abs(p - a);
          const pb = Math.abs(p - b);
          const pc = Math.abs(p - c);
          value += pa <= pb && pa <= pc ? a : pb <= pc ? b : c;
          break;
        }
      }
      cur[i] = value & 0xff;
    }
    for (let x = 0; x < width; x++) {
      const s = x * channels;
      const d = (y * width + x) * 4;
      pixels[d] = cur[s];
      pixels[d + 1] = cur[s + 1];
      pixels[d + 2] = cur[s + 2];
      pixels[d + 3] = channels === 4 ? cur[s + 3] : 255;
    }
    prev = cur;
  }
  return { width, height, pixels };
}

function boxResize(image, size) {
  const { width, height, pixels } = image;
  const outPixels = new Uint8Array(size * size * 4);
  const scaleX = width / size;
  const scaleY = height / size;
  for (let y = 0; y < size; y++) {
    const y0 = Math.floor(y * scaleY);
    const y1 = Math.max(y0 + 1, Math.floor((y + 1) * scaleY));
    for (let x = 0; x < size; x++) {
      const x0 = Math.floor(x * scaleX);
      const x1 = Math.max(x0 + 1, Math.floor((x + 1) * scaleX));
      let r = 0, g = 0, b = 0, a = 0, n = 0;
      for (let sy = y0; sy < y1; sy++) {
        for (let sx = x0; sx < x1; sx++) {
          const i = (sy * width + sx) * 4;
          const alpha = pixels[i + 3];
          r += pixels[i] * alpha;
          g += pixels[i + 1] * alpha;
          b += pixels[i + 2] * alpha;
          a += alpha;
          n++;
        }
      }
      const d = (y * size + x) * 4;
      if (a === 0) {
        outPixels[d + 3] = 0;
        continue;
      }
      outPixels[d] = Math.round(r / a);
      outPixels[d + 1] = Math.round(g / a);
      outPixels[d + 2] = Math.round(b / a);
      outPixels[d + 3] = Math.round(a / n);
    }
  }
  return outPixels;
}

// 图标大面积为纯色，每行都选上/左/Paeth 里残差最小的滤波，压缩率足够。
function encodePng(size, pixels) {
  const stride = size * 4;
  const raw = Buffer.alloc((stride + 1) * size);
  let prev = Buffer.alloc(stride);
  for (let y = 0; y < size; y++) {
    const line = Buffer.from(pixels.subarray(y * stride, (y + 1) * stride));
    const candidates = [];
    for (const filter of [0, 1, 2, 4]) {
      const filtered = Buffer.alloc(stride);
      let cost = 0;
      for (let i = 0; i < stride; i++) {
        const a = i >= 4 ? line[i - 4] : 0;
        const b = prev[i];
        const c = i >= 4 ? prev[i - 4] : 0;
        let predictor = 0;
        switch (filter) {
          case 1: predictor = a; break;
          case 2: predictor = b; break;
          case 4: {
            const p = a + b - c;
            const pa = Math.abs(p - a);
            const pb = Math.abs(p - b);
            const pc = Math.abs(p - c);
            predictor = pa <= pb && pa <= pc ? a : pb <= pc ? b : c;
            break;
          }
        }
        const value = (line[i] - predictor) & 0xff;
        filtered[i] = value;
        cost += value < 128 ? value : 256 - value;
      }
      candidates.push({ filter, filtered, cost });
    }
    candidates.sort((x, y2) => x.cost - y2.cost);
    raw[y * (stride + 1)] = candidates[0].filter;
    candidates[0].filtered.copy(raw, y * (stride + 1) + 1);
    prev = line;
  }

  const chunk = (type, data) => {
    const head = Buffer.alloc(8);
    head.writeUInt32BE(data.length, 0);
    head.write(type, 4, 'ascii');
    const crc = Buffer.alloc(4);
    crc.writeUInt32BE(crc32(Buffer.concat([Buffer.from(type, 'ascii'), data])) >>> 0, 0);
    return Buffer.concat([head, data, crc]);
  };

  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(size, 0);
  ihdr.writeUInt32BE(size, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 6; // RGBA
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

const image = decodePng(readFileSync(src));
const pixels = image.width === SIZE && image.height === SIZE ? image.pixels : boxResize(image, SIZE);
writeFileSync(out, encodePng(SIZE, pixels));
console.log(`${out} (${SIZE}x${SIZE})`);
