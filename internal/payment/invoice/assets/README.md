# Invoice PDF font

`NotoSans-Regular.ttf` is Noto Sans Regular, used by the invoice PDF because it
covers Vietnamese diacritics (Latin Extended Additional) and the Vietnamese
dong sign (U+20AB). It is embedded in the binary with `//go:embed`, so the
server needs no font directory at runtime.

- Source: <https://github.com/notofonts/noto-fonts/raw/main/hinted/ttf/NotoSans/NotoSans-Regular.ttf>
- Version: Noto Sans v2.013 (static, hinted TTF)
- Size: 569208 bytes
- SHA-256: `b85c38ecea8a7cfb39c24e395a4007474fa5a4fc864f6ee33309eb4948d232d5`
- Licence: SIL Open Font License, Version 1.1 — see `OFL.txt` in this directory
  (the same licence text as <https://github.com/google/fonts/raw/main/ofl/notosans/OFL.txt>,
  SHA-256 `cee9892f9f0cc8fe882c9e9537ee6a89621d86ee7ceaf70b02e2b2b1c25c061a`)

The font is redistributed unmodified; the OFL requires the licence text to
travel with it, which it does. The PDF renderer (`pdf.go`) embeds a subset of
the font in each generated document.

To verify the vendored file:

```sh
shasum -a 256 internal/payment/invoice/assets/NotoSans-Regular.ttf
```
