// Draws the app icon: the NodeEx mark, a polished sphere, on a dark tile.
// Run by scripts/make-icon.sh; the result is committed as Resources/AppIcon.icns.
import AppKit

let size: CGFloat = 1024
let image = NSImage(size: NSSize(width: size, height: size))
image.lockFocus()
let ctx = NSGraphicsContext.current!.cgContext
let space = CGColorSpaceCreateDeviceRGB()
func color(_ r: CGFloat, _ g: CGFloat, _ b: CGFloat, _ a: CGFloat = 1) -> CGColor { CGColor(colorSpace: space, components: [r, g, b, a])! }

// The tile, in the proportions macOS icons use.
let tile = CGRect(x: 100, y: 100, width: 824, height: 824)
let tilePath = CGPath(roundedRect: tile, cornerWidth: 186, cornerHeight: 186, transform: nil)
ctx.saveGState()
ctx.setShadow(offset: CGSize(width: 0, height: -12), blur: 28, color: color(0, 0, 0, 0.45))
ctx.addPath(tilePath)
ctx.setFillColor(color(0.04, 0.04, 0.05))
ctx.fillPath()
ctx.restoreGState()
ctx.saveGState()
ctx.addPath(tilePath)
ctx.clip()
ctx.drawLinearGradient(CGGradient(colorsSpace: space, colors: [color(0.13, 0.135, 0.15), color(0.035, 0.035, 0.045)] as CFArray, locations: [0, 1])!,
                       start: CGPoint(x: 512, y: 924), end: CGPoint(x: 512, y: 100), options: [])
// A faint glow behind the sphere.
ctx.drawRadialGradient(CGGradient(colorsSpace: space, colors: [color(1, 1, 1, 0.10), color(1, 1, 1, 0)] as CFArray, locations: [0, 1])!,
                       startCenter: CGPoint(x: 512, y: 512), startRadius: 0, endCenter: CGPoint(x: 512, y: 512), endRadius: 420, options: [])
ctx.restoreGState()
ctx.addPath(tilePath)
ctx.setStrokeColor(color(1, 1, 1, 0.10))
ctx.setLineWidth(3)
ctx.strokePath()

// The sphere: light from the upper left, a dark limb, a soft floor shadow.
let center = CGPoint(x: 512, y: 520)
let radius: CGFloat = 236
ctx.saveGState()
ctx.setShadow(offset: CGSize(width: 0, height: -26), blur: 60, color: color(0, 0, 0, 0.7))
ctx.addEllipse(in: CGRect(x: center.x - radius, y: center.y - radius, width: radius * 2, height: radius * 2))
ctx.setFillColor(color(0.3, 0.31, 0.33))
ctx.fillPath()
ctx.restoreGState()
ctx.saveGState()
ctx.addEllipse(in: CGRect(x: center.x - radius, y: center.y - radius, width: radius * 2, height: radius * 2))
ctx.clip()
ctx.drawRadialGradient(CGGradient(colorsSpace: space,
                                  colors: [color(1, 1, 1), color(0.86, 0.87, 0.9), color(0.5, 0.52, 0.56), color(0.17, 0.18, 0.2), color(0.07, 0.07, 0.08)] as CFArray,
                                  locations: [0, 0.18, 0.5, 0.82, 1])!,
                       startCenter: CGPoint(x: center.x - 84, y: center.y + 96), startRadius: 0,
                       endCenter: center, endRadius: radius * 1.06, options: [.drawsAfterEndLocation])
// Light bouncing back onto the underside.
ctx.drawRadialGradient(CGGradient(colorsSpace: space, colors: [color(1, 1, 1, 0.22), color(1, 1, 1, 0)] as CFArray, locations: [0, 1])!,
                       startCenter: CGPoint(x: center.x + 70, y: center.y - 170), startRadius: 0,
                       endCenter: CGPoint(x: center.x + 70, y: center.y - 170), endRadius: 170, options: [])
ctx.restoreGState()
image.unlockFocus()

let rep = NSBitmapImageRep(data: image.tiffRepresentation!)!
try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: CommandLine.arguments[1]))
