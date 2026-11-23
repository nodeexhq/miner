// Draws an app icon: the white NVTO mark on a flat tile. Run by
// scripts/make-icon.sh together with Sources/*/MenuBarMark.swift (which holds
// the mark). Arguments: OUTPUT.png TILE-HEX [avatar]. "avatar" fills the whole
// square, for places that crop the picture themselves; otherwise it is a macOS
// icon, a rounded tile with room around it.
import AppKit

let args = CommandLine.arguments
let hex = Int(args[2].dropFirst(), radix: 16)!
let tileColor = CGColor(srgbRed: CGFloat(hex >> 16 & 255) / 255, green: CGFloat(hex >> 8 & 255) / 255, blue: CGFloat(hex & 255) / 255, alpha: 1)
let avatar = args.count > 3 && args[3] == "avatar"

let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: 1024, pixelsHigh: 1024, bitsPerSample: 8, samplesPerPixel: 4,
                           hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
let ctx = NSGraphicsContext(bitmapImageRep: rep)!.cgContext
ctx.translateBy(x: 0, y: 1024); ctx.scaleBy(x: 1, y: -1) // y down, as SVG

let tile = avatar ? CGRect(x: 0, y: 0, width: 1024, height: 1024) : CGRect(x: 100, y: 100, width: 824, height: 824)
if !avatar {
    // The soft shadow macOS icons sit on.
    ctx.saveGState()
    ctx.setShadow(offset: CGSize(width: 0, height: 10), blur: 24, color: CGColor(gray: 0, alpha: 0.28))
    ctx.addPath(CGPath(roundedRect: tile, cornerWidth: 186, cornerHeight: 186, transform: nil))
    ctx.setFillColor(tileColor); ctx.fillPath()
    ctx.restoreGState()
} else {
    ctx.setFillColor(tileColor); ctx.fill(tile)
}
let markSize = tile.width * (avatar ? 0.42 : 0.56)
ctx.setFillColor(CGColor(gray: 1, alpha: 1))
NodeExMark.fill(in: CGRect(x: tile.midX - markSize / 2, y: tile.midY - markSize / 2, width: markSize, height: markSize), context: ctx)

try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: args[1]))
