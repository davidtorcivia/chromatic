// Minimal PNG reader. The Mac has no ffmpeg and installing a decoder on three
// machines is worse than 60 lines here. Handles what browsers actually emit:
// 8-bit non-interlaced RGB or RGBA.
import { inflateSync } from 'node:zlib';

export function decodePNG(buf) {
    if (buf.readUInt32BE(0) !== 0x89504e47) throw new Error('not a PNG');

    let pos = 8;
    let width = 0, height = 0, bitDepth = 0, colorType = 0;
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
            if (data[12] !== 0) throw new Error('interlaced PNG not supported');
        } else if (type === 'IDAT') {
            idat.push(data);
        } else if (type === 'IEND') {
            break;
        }
        pos += 12 + len;
    }

    if (bitDepth !== 8) throw new Error(`bit depth ${bitDepth} not supported`);
    // Read the channel count from IHDR rather than assuming RGBA: a colour-type
    // 2 screenshot would silently shift every sample by the wrong stride.
    const channels = colorType === 6 ? 4 : colorType === 2 ? 3 : 0;
    if (!channels) throw new Error(`colour type ${colorType} not supported`);

    const raw = inflateSync(Buffer.concat(idat));
    const stride = width * channels;
    const out = Buffer.alloc(height * stride);

    for (let y = 0; y < height; y++) {
        const filter = raw[y * (stride + 1)];
        const line = raw.subarray(y * (stride + 1) + 1, y * (stride + 1) + 1 + stride);
        const cur = out.subarray(y * stride, (y + 1) * stride);
        const prior = y > 0 ? out.subarray((y - 1) * stride, y * stride) : null;

        for (let i = 0; i < stride; i++) {
            const a = i >= channels ? cur[i - channels] : 0;
            const b = prior ? prior[i] : 0;
            const c = prior && i >= channels ? prior[i - channels] : 0;
            let val;
            switch (filter) {
                case 0: val = line[i]; break;
                case 1: val = line[i] + a; break;
                case 2: val = line[i] + b; break;
                case 3: val = line[i] + ((a + b) >> 1); break;
                case 4: {
                    const p = a + b - c;
                    const pa = Math.abs(p - a), pb = Math.abs(p - b), pc = Math.abs(p - c);
                    val = line[i] + (pa <= pb && pa <= pc ? a : pb <= pc ? b : c);
                    break;
                }
                default: throw new Error(`unknown filter ${filter} on row ${y}`);
            }
            cur[i] = val & 0xff;
        }
    }

    return { width, height, channels, data: out };
}

// Samples the three rows at the patch columns, scaling for Retina/HiDPI. The
// scale is asserted rather than assumed: a scrollbar-shifted or oddly scaled
// capture would sample the wrong pixels and read as a perfect 0/255 match.
export function samplePatches(png, cssWidth, xs, rows) {
    const scale = png.width / cssWidth;
    if (!Number.isInteger(scale) || scale < 1 || scale > 3) {
        throw new Error(`unexpected screenshot scale ${scale} (png ${png.width}x${png.height}, css width ${cssWidth})`);
    }
    const at = (x, y) => {
        const i = (Math.round(y * scale) * png.width + Math.round(x * scale)) * png.channels;
        return [png.data[i], png.data[i + 1], png.data[i + 2]];
    };
    const out = { scale };
    for (const [name, y] of Object.entries(rows)) out[name] = xs.map((x) => at(x, y));
    return out;
}

export const maxDelta = (a, b) => Math.max(...a.map((_, i) => Math.abs(a[i] - b[i])));
