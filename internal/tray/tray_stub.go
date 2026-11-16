//go:build !darwin && !windows

package tray

// stubController no-op 托盘（Linux 等暂无验证环境的平台）：保证编译过、调用不崩。
type stubController struct{}

// New 返回当前平台的托盘控制器。
func New() Controller { return &stubController{} }

func (stubController) Start([]byte, string, []Item) {}
func (stubController) Update([]Item)                {}
func (stubController) SetDockVisible(bool)          {}
func (stubController) Quit()                        {}
