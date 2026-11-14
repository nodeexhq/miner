// Draws the app icon: the NodeEx mark, a polished sphere inside an orbit, on a dark tile.
// Run by scripts/make-icon.sh; the result is committed as Resources/AppIcon.icns.
import AppKit

let size: CGFloat = 1024
let image = NSImage(size: NSSize(width: size, height: size))
image.lockFocus()
let ctx = NSGraphicsContext.current!.cgContext
let space = CGColorSpaceCreateDeviceRGB()
func color(_ r: CGFloat, _ g: CGFloat, _ b: CGFloat, _ a: CGFloat = 1) -> CGColor { CGColor(colorSpace: space, components: [r, g, b, a])! }

// "avatar" as a second argument draws the mark edge to edge on a square,
// for places that crop the picture themselves (a GitHub avatar); otherwise
// it is a macOS icon, a rounded tile with room around it.
let avatar = CommandLine.arguments.count > 2 && CommandLine.arguments[2] == "avatar"

// The tile, in the proportions macOS icons use.
let tile = avatar ? CGRect(x: 0, y: 0, width: 1024, height: 1024) : CGRect(x: 100, y: 100, width: 824, height: 824)
let tilePath = avatar ? CGPath(rect: tile, transform: nil) : CGPath(roundedRect: tile, cornerWidth: 186, cornerHeight: 186, transform: nil)
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
                       start: CGPoint(x: 512, y: tile.maxY), end: CGPoint(x: 512, y: tile.minY), options: [.drawsBeforeStartLocation, .drawsAfterEndLocation])
// A faint glow behind the sphere.
ctx.drawRadialGradient(CGGradient(colorsSpace: space, colors: [color(1, 1, 1, 0.10), color(1, 1, 1, 0)] as CFArray, locations: [0, 1])!,
                       startCenter: CGPoint(x: 512, y: 512), startRadius: 0, endCenter: CGPoint(x: 512, y: 512), endRadius: 420, options: [])
ctx.restoreGState()
if !avatar {
    ctx.addPath(tilePath)
    ctx.setStrokeColor(color(1, 1, 1, 0.10))
    ctx.setLineWidth(3)
    ctx.strokePath()
}

// The mark: a sphere with an orbit around it, a node and the network it
// belongs to. The orbit is a tilted ellipse; its far half is drawn before
// the sphere and its near half after, so the sphere sits inside it.
let center = CGPoint(x: 512, y: 512)
// GitHub shows avatars as circles in places, so the mark must sit inside
// the inscribed circle with room to spare.
let markScale: CGFloat = avatar ? 1.0 : 1.0
let radius: CGFloat = 196 * markScale
let orbit = CGRect(x: -330 * markScale, y: -112 * markScale, width: 660 * markScale, height: 224 * markScale)
let tilt: CGFloat = -0.42

// The whole orbit is drawn first and the sphere over it, which hides the
// part that passes behind; the near half is then drawn again on top. Both
// passes use one colour, so the ring has no seam where the halves meet.
func strokeOrbit(nearHalfOnly: Bool) {
    ctx.saveGState()
    ctx.translateBy(x: center.x, y: center.y)
    ctx.rotate(by: tilt)
    if nearHalfOnly {
        ctx.clip(to: CGRect(x: -400, y: -200, width: 800, height: 200))
    }
    ctx.addEllipse(in: orbit)
    ctx.setLineWidth(18)
    ctx.setStrokeColor(color(0.9, 0.91, 0.94))
    ctx.strokePath()
    ctx.restoreGState()
}

strokeOrbit(nearHalfOnly: false)

// The sphere: light from the upper left, a dark limb, a soft floor shadow.
ctx.saveGState()
ctx.setShadow(offset: CGSize(width: 0, height: -22), blur: 52, color: color(0, 0, 0, 0.7))
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
                       startCenter: CGPoint(x: center.x - 70, y: center.y + 80), startRadius: 0,
                       endCenter: center, endRadius: radius * 1.06, options: [.drawsAfterEndLocation])
ctx.drawRadialGradient(CGGradient(colorsSpace: space, colors: [color(1, 1, 1, 0.2), color(1, 1, 1, 0)] as CFArray, locations: [0, 1])!,
                       startCenter: CGPoint(x: center.x + 58, y: center.y - 140), startRadius: 0,
                       endCenter: CGPoint(x: center.x + 58, y: center.y - 140), endRadius: 140, options: [])
ctx.restoreGState()

strokeOrbit(nearHalfOnly: true)

// A second, small node travelling on the orbit.
ctx.saveGState()
ctx.translateBy(x: center.x, y: center.y)
ctx.rotate(by: tilt)
let small = CGPoint(x: 232, y: -79)
ctx.setShadow(offset: CGSize(width: 0, height: -6), blur: 16, color: color(0, 0, 0, 0.6))
ctx.addEllipse(in: CGRect(x: small.x - 40, y: small.y - 40, width: 80, height: 80))
ctx.setFillColor(color(0.97, 0.97, 0.99))
ctx.fillPath()
ctx.restoreGState()
image.unlockFocus()

let rep = NSBitmapImageRep(data: image.tiffRepresentation!)!
try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: CommandLine.arguments[1]))
