// MakeIcon — draws dbc.app's icon into an .iconset directory, for iconutil.
//
//	swiftc MakeIcon.swift -o makeicon -framework AppKit
//	./makeicon dbc.iconset && iconutil -c icns dbc.iconset -o dbc.icns
//
// Drawn in code rather than kept as a PNG so it follows the theme's two
// colors (theme.Bg, theme.Accent — the favicon's) and needs no binary in the
// repo. A macOS squircle of the dark background, a database cylinder in the
// accent, and "dbc" under it in the favicon's monospace.
//
// Offscreen AppKit drawing needs a window-server connection, so this fails
// over SSH; mac-install.sh then installs the app without an icon.

import AppKit
import Foundation

let bg = NSColor(srgbRed: 0x1f / 255.0, green: 0x24 / 255.0, blue: 0x20 / 255.0, alpha: 1)
let bgLight = NSColor(srgbRed: 0x2e / 255.0, green: 0x38 / 255.0, blue: 0x31 / 255.0, alpha: 1)
let accent = NSColor(srgbRed: 0x4d / 255.0, green: 0xb3 / 255.0, blue: 0x80 / 255.0, alpha: 1)
let accentDark = NSColor(srgbRed: 0x2f / 255.0, green: 0x7d / 255.0, blue: 0x57 / 255.0, alpha: 1)

func draw(_ size: CGFloat) {
    let full = CGRect(x: 0, y: 0, width: size, height: size)
    NSColor.clear.set()
    full.fill()

    // The squircle, inset as Apple's template insets it, lit from the top.
    let r = full.insetBy(dx: size * 0.09, dy: size * 0.09)
    let plate = NSBezierPath(roundedRect: r, xRadius: r.width * 0.2237, yRadius: r.height * 0.2237)
    NSGraphicsContext.saveGraphicsState()
    plate.addClip()
    NSGradient(starting: bgLight, ending: bg)?.draw(in: r, angle: -90)
    NSGraphicsContext.restoreGraphicsState()

    // The cylinder: a body with a rim ellipse on top and two band lines —
    // the database glyph everyone reads at a glance, even at 16px.
    let cw = r.width * 0.50
    let cx = r.midX - cw / 2
    let eh = cw * 0.32 // ellipse height
    let top = r.minY + r.height * 0.80
    let bottom = r.minY + r.height * 0.38
    // the body is a rectangle plus the bottom ellipse, filled as one clip
    NSGraphicsContext.saveGraphicsState()
    let bodyShape = NSBezierPath(rect: CGRect(x: cx, y: bottom, width: cw, height: top - eh / 2 - bottom))
    bodyShape.append(NSBezierPath(ovalIn: CGRect(x: cx, y: bottom - eh / 2, width: cw, height: eh)))
    bodyShape.addClip()
    NSGradient(starting: accent, ending: accentDark)?.draw(in: CGRect(x: cx, y: bottom - eh / 2, width: cw, height: top - bottom + eh), angle: 0)
    NSGraphicsContext.restoreGraphicsState()

    // bands: the lower half of an ellipse, in the background colour
    bg.withAlphaComponent(0.55).setStroke()
    for i in 1...2 {
        let y = top - eh / 2 - (top - eh / 2 - bottom) * CGFloat(i) / 3
        let band = NSBezierPath()
        band.appendArc(withCenter: .zero, radius: 1, startAngle: 180, endAngle: 360)
        var t = AffineTransform(translationByX: r.midX, byY: y)
        t.scale(x: cw / 2, y: eh / 2)
        band.transform(using: t)
        band.lineWidth = max(1, size * 0.012)
        band.stroke()
    }

    // the top face, lighter, with a dark rim
    let lid = NSBezierPath(ovalIn: CGRect(x: cx, y: top - eh, width: cw, height: eh))
    NSColor(srgbRed: 0x7f / 255.0, green: 0xd1 / 255.0, blue: 0xa6 / 255.0, alpha: 1).setFill()
    lid.fill()
    accentDark.setStroke()
    lid.lineWidth = max(1, size * 0.010)
    lid.stroke()

    // "dbc" below — dropped at the smallest sizes, where it would be a smudge
    if size >= 64 {
        let font = NSFont.monospacedSystemFont(ofSize: r.height * 0.17, weight: .bold)
        let attrs: [NSAttributedString.Key: Any] = [.font: font, .foregroundColor: accent]
        let text = NSAttributedString(string: "dbc", attributes: attrs)
        let ts = text.size()
        text.draw(at: CGPoint(x: r.midX - ts.width / 2, y: r.minY + r.height * 0.07))
    }
}

func png(_ px: Int) -> Data {
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: px, pixelsHigh: px,
                               bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                               colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
    draw(CGFloat(px))
    NSGraphicsContext.restoreGraphicsState()
    return rep.representation(using: .png, properties: [:])!
}

guard CommandLine.arguments.count == 2 else {
    FileHandle.standardError.write(Data("usage: makeicon DIR.iconset\n".utf8))
    exit(2)
}
let outDir = URL(fileURLWithPath: CommandLine.arguments[1])
try FileManager.default.createDirectory(at: outDir, withIntermediateDirectories: true)
let targets: [(String, Int)] = [
    ("icon_16x16.png", 16), ("icon_16x16@2x.png", 32),
    ("icon_32x32.png", 32), ("icon_32x32@2x.png", 64),
    ("icon_128x128.png", 128), ("icon_128x128@2x.png", 256),
    ("icon_256x256.png", 256), ("icon_256x256@2x.png", 512),
    ("icon_512x512.png", 512), ("icon_512x512@2x.png", 1024),
]
for (name, px) in targets {
    try png(px).write(to: outDir.appendingPathComponent(name))
}
