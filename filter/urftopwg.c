/*
 * urftopwg.c v13 — Apple URF -> CUPS Raster v3（标准 sync + page_header2 布局）
 * 600dpi->300dpi 2x2 平均缩放；页尺寸用浮点、奇数行高度向上取整；纯 C 流式
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

static uint32_t rd32be(const unsigned char *p){ return ((uint32_t)p[0]<<24)|((uint32_t)p[1]<<16)|((uint32_t)p[2]<<8)|(uint32_t)p[3]; }
static void wr32le(unsigned char *p, uint32_t v){ p[0]=(unsigned char)(v&255); p[1]=(unsigned char)((v>>8)&255); p[2]=(unsigned char)((v>>16)&255); p[3]=(unsigned char)((v>>24)&255); }
static void wr32be(unsigned char *p, uint32_t v){ p[0]=(unsigned char)(v>>24); p[1]=(unsigned char)((v>>16)&255); p[2]=(unsigned char)((v>>8)&255); p[3]=(unsigned char)(v&255); }

static void avg_rows(unsigned char *out, const unsigned char *r0, const unsigned char *r1, unsigned sw, int bpp){
    for(unsigned x = 0; x < sw; x++){
        unsigned i0 = x * (unsigned)bpp * 2;
        for(int c = 0; c < bpp; c++){
            unsigned v = (unsigned)r0[i0+c] + (unsigned)r0[i0+(unsigned)bpp+c] + (unsigned)r1[i0+c] + (unsigned)r1[i0+(unsigned)bpp+c];
            out[x*(unsigned)bpp+c] = (unsigned char)(v >> 2);
        }
    }
}

int main(int argc, char **argv){
    FILE *in = stdin, *out = stdout;
    if(argc >= 7 && strcmp(argv[6], "-") != 0) in = fopen(argv[6], "rb");
    else if(argc == 3){ in = fopen(argv[1], "rb"); out = fopen(argv[2], "wb"); }
    if(!in || !out){ fprintf(stderr, "open failed\n"); return 1; }
    unsigned char hdr[8];
    if(fread(hdr, 1, 8, in) != 8 || memcmp(hdr, "UNIRAST", 7) != 0 || hdr[7] != 0){
        fprintf(stderr, "bad URF magic\n"); return 1;
    }
    fread(hdr, 1, 4, in);
    int first_page = 1;
    for(;;){
        unsigned char ph[32];
        if(fread(ph, 1, 32, in) != 32) break;
        unsigned bits = ph[0], cspace = ph[1];
        uint32_t width = rd32be(ph+12), height = rd32be(ph+16), hres = rd32be(ph+20);
        if(width == 0 || height == 0 || bits == 0 || width > 20000 || height > 20000) break;
        int bpp = (int)((bits + 7) / 8);
        uint32_t bpl = width * (uint32_t)bpp;
        unsigned bpc = bits / (cspace == 0 ? 1 : 3);
        unsigned cs  = cspace == 0 ? 18 : 19;
        unsigned nc  = cspace == 0 ? 1 : 3;
        unsigned scale = (hres >= 600) ? 2 : 1;
        uint32_t sw = width / scale, sh = (height + scale - 1) / scale;
        uint32_t out_bpl = sw * (uint32_t)bpp;
        unsigned dpi_out = hres / scale;
        float page_w_f = (float)sw * 72.0f / (float)dpi_out;
        float page_h_f = (float)sh * 72.0f / (float)dpi_out;
        unsigned page_w = (unsigned)page_w_f, page_h = (unsigned)page_h_f;

        /* 标准 v3 头：sync(4) + cups_page_header2_t(1796)，page_header2 从文件 4 */
        unsigned char h[1800];
        memset(h, 0, sizeof(h));
        /* sync：CUPS_RASTER_SYNC=0x52615333 的 LE 字节（CUPS 写模式同此） */
        h[0]=0x33; h[1]=0x53; h[2]=0x61; h[3]=0x52;
        #define P 4
        wr32le(h+P+272, 0);                          /* Duplex */
        wr32le(h+P+276, dpi_out); wr32le(h+P+280, dpi_out);   /* HWResolution */
        wr32le(h+P+352, page_w); wr32le(h+P+356, page_h);     /* PageSize */
        wr32le(h+P+372, sw);                         /* cupsWidth */
        wr32le(h+P+376, sh);                         /* cupsHeight */
        wr32le(h+P+380, 0);                          /* cupsMediaType */
        wr32le(h+P+384, bpc);                        /* cupsBitsPerColor */
        wr32le(h+P+388, bits);                       /* cupsBitsPerPixel */
        wr32le(h+P+392, out_bpl);                    /* cupsBytesPerLine */
        wr32le(h+P+396, 0);                          /* cupsColorOrder */
        wr32le(h+P+400, cs);                         /* cupsColorSpace */
        wr32le(h+P+404, 0);                          /* cupsCompression */
        wr32le(h+P+408, 0);                          /* cupsRowCount */
        wr32le(h+P+412, 0);                          /* cupsRowFeed */
        wr32le(h+P+416, 0);                          /* cupsRowStep */
        wr32le(h+P+420, nc);                         /* cupsNumColors */
        memcpy(h+P+428, &page_w_f, 4); memcpy(h+P+432, &page_h_f, 4);   /* cupsPageSize */
        /* CUPS raster：sync 只在文件头写一次，之后每页只写 1796 字节页头 */
        if(first_page){
            fwrite(h, 1, sizeof(h), out);
            first_page = 0;
        } else {
            fwrite(h + 4, 1, sizeof(h) - 4, out);
        }

        unsigned char *line = (unsigned char *)malloc(bpl);
        unsigned char *rows[2];
        rows[0] = (unsigned char *)malloc(bpl);
        rows[1] = (unsigned char *)malloc(bpl);
        unsigned char *outrow = (unsigned char *)malloc(out_bpl);
        if(!line || !rows[0] || !rows[1] || !outrow){ fprintf(stderr, "oom\n"); return 1; }

        uint32_t written = 0;
        while(written < height){
            int rb = fgetc(in); if(rb < 0) break;
            int repeat = rb + 1;
            uint32_t pos = 0;
            while(pos < bpl){
                int cmd = fgetc(in);
                if(cmd < 0){ fprintf(stderr, "truncated packbits\n"); goto done; }
                if(cmd <= 0x7f){
                    unsigned n = (unsigned)(cmd + 1) * (unsigned)bpp;
                    if(pos + n > bpl) n = bpl - pos;
                    unsigned char px[16];
                    if(fread(px, 1, (size_t)bpp, in) != (size_t)bpp){ fprintf(stderr, "truncated repeat px\n"); goto done; }
                    for(unsigned i = 0; i < n; i++) line[pos+i] = px[i % (unsigned)bpp];
                    pos += n;
                } else if(cmd == 0x80){
                    memset(line + pos, 0xff, (size_t)(bpl - pos));
                    pos = bpl;
                } else {
                    unsigned n = (unsigned)(257 - cmd) * (unsigned)bpp;
                    if(pos + n > bpl) n = bpl - pos;
                    if(fread(line + pos, 1, (size_t)n, in) != (size_t)n){ fprintf(stderr, "truncated verbatim\n"); goto done; }
                    pos += n;
                }
            }
            for(int k = 0; k < repeat && written < height; k++){
                if(scale == 2){
                    memcpy(rows[written % 2], line, bpl);
                    written++;
                    if(written % 2 == 0){
                        avg_rows(outrow, rows[0], rows[1], sw, bpp);
                        fwrite(outrow, 1, out_bpl, out);
                    }
                } else {
                    fwrite(line, 1, bpl, out);
                    written++;
                }
            }
        }
        /* 奇数高度：用最后一行补足输出最后一行 */
        if(scale == 2 && (height % 2 == 1) && (written == height)){
            memcpy(rows[1], rows[0], bpl);
            avg_rows(outrow, rows[0], rows[1], sw, bpp);
            fwrite(outrow, 1, out_bpl, out);
        }
done:
        free(line); free(rows[0]); free(rows[1]); free(outrow);
    }
    fflush(out);
    fprintf(stderr, "converted\n");
    return 0;
}
