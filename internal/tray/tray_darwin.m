//go:build darwin

// NSStatusItem 托盘 shim：所有 UI 操作派发到主队列执行（已在主线程则直接执行），
// 不注册 NSApplicationDelegate、不启动/嵌套事件循环，与 Wails 的 NSApp 循环共存。
//
// 菜单用单个持久 NSMenu 原地重建（removeAllItems + 重加）：
//   - menuWillOpen 里同步重建，保证展开前内容就是最新的（该回调的设计用途）；
//   - 菜单打开期间收到的更新先挂起，menuDidClose 后再套用——
//     打开中的菜单整体换内容会被 AppKit 取消展示（左键点开菜单消失的根因）。
#import <Cocoa/Cocoa.h>
#import <Foundation/Foundation.h>
#import <objc/runtime.h>

extern void trayItemClicked(const char *action);
extern void trayMenuWillOpen(void);
extern void trayAppQuitRequested(void);
extern void trayDockIconClicked(void);

// 区分「应用级退出」与「窗口关闭」：Wails 在 macOS 上把红叉关闭（WindowDelegate
// windowShouldClose）和 Dock 退出 / Cmd+Q（applicationShouldTerminate）都收敛成
// processMessage("Q") → OnBeforeClose，Go 层无法区分。这里 swizzle Wails 的
// applicationShouldTerminate，先置「应用级退出请求」标志再调原实现，
// Go 侧 OnBeforeClose 据此走真退出而不是询问/最小化。
//
// ⚠ 类名随 wails 版本变化：v2.16 起 @implementation AppDelegate（老版本叫
// WailsAppDelegate），两个都尝试——类名对不上 swizzle 会静默失效，
// 表现为「Dock 退出/Cmd+Q 被当成窗口关闭，最小化进托盘」（v0.3.0 实测踩坑）。
static IMP orig_shouldTerminate = NULL;
static NSApplicationTerminateReply swz_shouldTerminate(id self, SEL _cmd, NSApplication *sender) {
    trayAppQuitRequested();
    if (orig_shouldTerminate == NULL) {
        return NSTerminateCancel; // 没钩到原实现时保持 wails 默认（cancel + 走 OnBeforeClose）
    }
    return ((NSApplicationTerminateReply (*)(id, SEL, id))orig_shouldTerminate)(self, _cmd, sender);
}

// Dock 图标点击（应用已运行时再次点击 Dock 图标）：wails 未实现
// applicationShouldHandleReopen，点击后什么都不发生（托盘态窗口不出来）。
// 动态补上：无可见窗口时通知 Go 恢复主窗口；有可见窗口走系统默认（置前）。
static BOOL swz_handleReopen(id self, SEL _cmd, NSApplication *sender, BOOL hasVisibleWindows) {
    if (!hasVisibleWindows) {
        trayDockIconClicked();
    }
    return YES;
}

__attribute__((constructor)) static void zcasSwizzleWailsTerminate(void) {
    // v2.16 类名是 AppDelegate，老版本是 WailsAppDelegate；按优先级取第一个命中的
    Class c = NSClassFromString(@"AppDelegate") ?: NSClassFromString(@"WailsAppDelegate");
    if (!c) {
        return;
    }
    Method m = class_getInstanceMethod(c, @selector(applicationShouldTerminate:));
    if (m) {
        orig_shouldTerminate = method_setImplementation(m, (IMP)swz_shouldTerminate);
    }
    // wails 未实现 reopen（已验证 v2.16 源码），安全补挂；已实现则不动
    if (!class_getInstanceMethod(c, @selector(applicationShouldHandleReopen:hasVisibleWindows:))) {
        class_addMethod(c, @selector(applicationShouldHandleReopen:hasVisibleWindows:),
                        (IMP)swz_handleReopen, "B@:@B");
    }
}

// ClickTarget 菜单项 target/action，把 action 字符串回传给 Go。
@interface ClickTarget : NSObject
- (void)onTrayItem:(NSMenuItem *)sender;
@end

// MenuHeaderView 分组标题行：左侧标题 + 右侧灰色小字提示（如「账号额度 … 点击切换」）。
// 纯展示不可点击，无悬浮高亮——与普通禁用项的观感一致。
@interface MenuHeaderView : NSView
@property (copy) NSString *title;
@property (copy) NSString *hint;
@end

@implementation MenuHeaderView
- (void)drawRect:(NSRect)dirtyRect {
    NSRect b = self.bounds;
    NSDictionary *titleAttrs = @{
        NSFontAttributeName: [NSFont menuFontOfSize:12],
        NSForegroundColorAttributeName: NSColor.secondaryLabelColor
    };
    NSDictionary *hintAttrs = @{
        NSFontAttributeName: [NSFont menuFontOfSize:11],
        NSForegroundColorAttributeName: NSColor.tertiaryLabelColor
    };
    [self.title drawInRect:NSMakeRect(8, 4.5, b.size.width - 16, 15) withAttributes:titleAttrs];
    CGSize hs = [self.hint sizeWithAttributes:hintAttrs];
    [self.hint drawInRect:NSMakeRect(b.size.width - 8 - hs.width, 5.5, hs.width + 2, 14)
           withAttributes:hintAttrs];
}
@end

// QuotaBarView 账号额度菜单项的自定义视图：账号名 + 百分比，下方一整行小高度圆角进度条
//（配色与主界面一致：余量充足绿 / 偏低黄 / 告急红）。有 action 时点击可触发。
@interface QuotaBarView : NSView
@property (copy) NSString *title;
@property (copy) NSString *action;
@property (assign) double usedPct; // 已用百分比 0-100（与主界面口径一致）
@property (assign) BOOL current;   // 当前账号：名称右侧画绿色圆点
@end

@implementation QuotaBarView
- (BOOL)acceptsFirstMouse:(NSEvent *)e { return YES; }

- (void)mouseUp:(NSEvent *)event {
    if (self.action.length > 0) {
        trayItemClicked([self.action UTF8String]);
        [self.enclosingMenuItem.menu cancelTracking];
    }
}

- (void)drawRect:(NSRect)dirtyRect {
    NSRect b = self.bounds;
    // 悬浮高亮：view 菜单项不享受 AppKit 的自动高亮（自绘绕过了普通项的绘制路径），
    // 这里查 enclosingMenuItem 的 isHighlighted 自绘选中背景 + 白字，
    // 与「恢复窗口」等普通项的可点击观感对齐；仅可点击行（action 非空）响应。
    BOOL hot = self.action.length > 0 && self.enclosingMenuItem.isHighlighted;
    if (hot) {
        [NSColor.selectedContentBackgroundColor setFill];
        NSBezierPath *bg = [NSBezierPath bezierPathWithRect:b];
        [bg fill];
    }
    NSDictionary *nameAttrs = @{
        NSFontAttributeName: [NSFont menuFontOfSize:12],
        NSForegroundColorAttributeName: hot ? NSColor.whiteColor : NSColor.labelColor
    };
    [self.title drawInRect:NSMakeRect(8, b.size.height - 18, b.size.width - 56, 16)
            withAttributes:nameAttrs];
    if (self.current) {
        // 当前账号标记：名称右侧的小绿点（与进度条充足色一致）；
        // 当前行 action 为空、永不进入高亮，无需适配蓝底
        CGFloat tx = 8 + [self.title sizeWithAttributes:nameAttrs].width + 6;
        NSRect dot = NSMakeRect(tx, b.size.height - 13.5, 6, 6);
        NSBezierPath *p = [NSBezierPath bezierPathWithOvalInRect:dot];
        [NSColor.systemGreenColor setFill];
        [p fill];
    }
    // 口径与主界面一致：显示「已用 xx%」，进度条填充量 = 已用占比
    NSString *pctText = [NSString stringWithFormat:@"已用 %.0f%%", self.usedPct];
    CGSize pctSize = [pctText sizeWithAttributes:nameAttrs];
    [pctText drawInRect:NSMakeRect(b.size.width - 8 - pctSize.width, b.size.height - 18,
                                   pctSize.width + 2, 16)
         withAttributes:nameAttrs];

    // 进度条：轨道浅灰圆角（高亮时换半透明白，灰字色在蓝底上发闷），填充色按余量分级
    //（对应主界面 pct-low/mid/high；绿/黄/红在选中蓝底上依然清晰，不用换）
    NSRect bar = NSMakeRect(8, 7, b.size.width - 16, 5);
    NSBezierPath *track = [NSBezierPath bezierPathWithRoundedRect:bar xRadius:2.5 yRadius:2.5];
    NSColor *trackColor = NSColor.quaternaryLabelColor;
    if (hot) {
        trackColor = [NSColor.whiteColor colorWithAlphaComponent:0.35];
    }
    [trackColor setFill];
    [track fill];
    double fill = self.usedPct / 100.0;
    if (fill < 0) fill = 0;
    if (fill > 1) fill = 1;
    if (fill > 0) {
        NSRect fillRect = bar;
        fillRect.size.width = bar.size.width * fill;
        // 按使用率配色（与主界面 pct-* 同阈值：<60% 绿、<85% 黄、其余红）
        NSColor *c = NSColor.systemRedColor;
        if (self.usedPct < 60) {
            c = NSColor.systemGreenColor;
        } else if (self.usedPct < 85) {
            c = NSColor.systemYellowColor;
        }
        NSBezierPath *p = [NSBezierPath bezierPathWithRoundedRect:fillRect xRadius:2.5 yRadius:2.5];
        [c setFill];
        [p fill];
    }
}
@end

@interface TrayMenuDelegate : NSObject <NSMenuDelegate>
- (void)menuWillOpen:(NSMenu *)menu;
- (void)menuDidClose:(NSMenu *)menu;
@end

static NSStatusItem *statusItem = nil;
static NSMenu *menu = nil;          // 持久菜单，原地重建
static TrayMenuDelegate *menuDelegate = nil;
static ClickTarget *clickTarget = nil;
static NSString *pendingMenu = nil; // 菜单打开期间到达的更新，关闭后套用
static bool menuOpen = false;
static bool inWillOpen = false; // menuWillOpen 回调执行中：此期间的重建必须立即生效

static void applyMenuIfClosed(NSString *menuJSON);
static void applyMenuNow(NSString *menuJSON);

@implementation ClickTarget
- (void)onTrayItem:(NSMenuItem *)sender {
    NSString *action = [sender representedObject];
    if (action.length > 0) {
        trayItemClicked([action UTF8String]);
    }
}
@end

@implementation TrayMenuDelegate
- (void)menuWillOpen:(NSMenu *)m {
    menuOpen = true;
    inWillOpen = true;
    trayMenuWillOpen(); // Go 侧同步重建菜单（必须同步：返回后才展开）
    inWillOpen = false;
}

- (void)menuDidClose:(NSMenu *)m {
    menuOpen = false;
    if (pendingMenu != nil) {
        NSString *p = pendingMenu;
        pendingMenu = nil;
        applyMenuNow(p);
    }
}
@end

// addItems 从 JSON（internal/tray.Item 结构）递归填充 menu。在主线程调用。
static void addItems(NSMenu *menu, NSArray *items) {
    for (NSDictionary *d in items) {
        if ([d[@"separator"] boolValue]) {
            [menu addItem:[NSMenuItem separatorItem]];
            continue;
        }
        NSString *title = d[@"title"] ?: @"";
        NSString *hint = d[@"hint"];
        if (hint.length > 0) {
            // 标题行带次级提示：左侧标题 + 右侧灰色小字（MenuHeaderView 自绘）
            MenuHeaderView *v = [[MenuHeaderView alloc] initWithFrame:NSMakeRect(0, 0, 250, 24)];
            v.title = title;
            v.hint = hint;
            NSMenuItem *mi = [[NSMenuItem alloc] init];
            [mi setView:v];
            [menu addItem:mi];
            continue;
        }
        id barVal = d[@"bar"];
        if (barVal != nil && [barVal respondsToSelector:@selector(doubleValue)]) {
            // 额度行：账号名 + 一整行小进度条（QuotaBarView 自绘），custom view 菜单项
            // 不走 target/action，点击由视图的 mouseUp 处理
            QuotaBarView *v = [[QuotaBarView alloc] initWithFrame:NSMakeRect(0, 0, 250, 34)];
            v.title = title;
            v.usedPct = [barVal doubleValue];
            v.current = [d[@"checked"] boolValue];
            v.action = d[@"action"] ?: @"";
            NSMenuItem *mi = [[NSMenuItem alloc] init];
            [mi setView:v];
            [menu addItem:mi];
            continue;
        }
        NSMenuItem *mi = [[NSMenuItem alloc] initWithTitle:title
                                                     action:NULL
                                              keyEquivalent:@""];
        if ([d[@"disabled"] boolValue]) {
            [mi setEnabled:NO];
        }
        if ([d[@"checked"] boolValue]) {
            [mi setState:NSControlStateValueOn];
        }
        NSString *tooltip = d[@"tooltip"];
        if (tooltip.length > 0) {
            [mi setToolTip:tooltip];
        }
        NSString *action = d[@"action"];
        if (action.length > 0) {
            [mi setTarget:clickTarget];
            [mi setAction:@selector(onTrayItem:)];
            [mi setRepresentedObject:action];
        }
        NSArray *children = d[@"children"];
        if (children.count > 0) {
            NSMenu *sub = [[NSMenu alloc] init];
            [sub setAutoenablesItems:NO];
            addItems(sub, children);
            [mi setSubmenu:sub];
        }
        [menu addItem:mi];
    }
}

// applyMenuNow 原地重建持久菜单（menuWillOpen 里同步调用，展开前生效）。在主线程调用。
static void applyMenuNow(NSString *menuJSON) {
    NSData *data = [menuJSON dataUsingEncoding:NSUTF8StringEncoding];
    NSError *err = nil;
    NSArray *items = [NSJSONSerialization JSONObjectWithData:data
                                                    options:0
                                                      error:&err];
    if (err != nil || ![items isKindOfClass:[NSArray class]]) {
        return;
    }
    [menu removeAllItems];
    addItems(menu, items);
}

// applyMenuIfClosed 菜单没打开时立即重建；正打开时挂起，待 menuDidClose 后套用
// （menuWillOpen 回调期间的重建除外：那是为本次展开准备的，必须立即生效）。
static void applyMenuIfClosed(NSString *menuJSON) {
    if (menuOpen && !inWillOpen) {
        pendingMenu = [menuJSON copy];
        return;
    }
    applyMenuNow(menuJSON);
}

static void onMainThread(dispatch_block_t block) {
    if ([NSThread isMainThread]) {
        block();
    } else {
        dispatch_async(dispatch_get_main_queue(), block);
    }
}

void tray_create(const char *icon, int iconLen, bool template, const char *tooltip, const char *menuJSON) {
    // 先把 C 指针转成 ObjC 对象再进 block：block 派发到主队列是异步的，
    // 而 Go 侧调用返回后会立即 C.free 这些指针——裸指针进 block 是悬垂引用
    //（wails dev 下崩溃 'data parameter is nil' 的根因）。
    NSData *iconData = [NSData dataWithBytes:icon length:iconLen];
    NSString *tooltipStr = tooltip != NULL ? [NSString stringWithUTF8String:tooltip] : nil;
    NSString *menuStr = [NSString stringWithUTF8String:menuJSON];
    onMainThread(^{
        if (menuDelegate == nil) {
            menuDelegate = [[TrayMenuDelegate alloc] init];
        }
        if (clickTarget == nil) {
            clickTarget = [[ClickTarget alloc] init];
        }
        if (menu == nil) {
            menu = [[NSMenu alloc] init];
            [menu setAutoenablesItems:NO];
            [menu setDelegate:menuDelegate];
        }
        if (statusItem == nil) {
            statusItem = [[NSStatusBar systemStatusBar]
                statusItemWithLength:NSVariableStatusItemLength];
            NSImage *img = [[NSImage alloc] initWithData:iconData];
            if (img != nil) {
                [img setSize:NSMakeSize(16, 16)];
                [img setTemplate:template ? YES : NO];
                statusItem.button.image = img;
            }
            if (tooltipStr != nil) {
                statusItem.button.toolTip = tooltipStr;
            }
            [statusItem setMenu:menu];
        }
        applyMenuIfClosed(menuStr);
    });
}

void tray_set_menu(const char *menuJSON) {
    NSString *json = [NSString stringWithUTF8String:menuJSON];
    onMainThread(^{
        applyMenuIfClosed(json);
    });
}

void tray_set_dock_visible(bool visible) {
    onMainThread(^{
        [NSApp setActivationPolicy:visible ? NSApplicationActivationPolicyRegular
                                        : NSApplicationActivationPolicyAccessory];
        if (visible) {
            [NSApp activateIgnoringOtherApps:YES];
        }
    });
}

void tray_destroy(void) {
    onMainThread(^{
        if (statusItem != nil) {
            [[NSStatusBar systemStatusBar] removeStatusItem:statusItem];
            statusItem = nil;
        }
    });
}

