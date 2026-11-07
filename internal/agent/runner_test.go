package agent

import (
	"strings"
	"testing"

	"laborlaw/internal/port"
)

func TestValidateInput(t *testing.T) {
	tests := []struct {
		name      string
		scene     string
		input     string
		wantCode  port.ErrorCode
		wantValid bool
	}{
		{
			name:      "正常问题",
			scene:     consultScene,
			input:     "公司让我试用期三个月，合法吗",
			wantCode:  port.CodeOK,
			wantValid: true,
		},
		{
			name:      "场景不支持",
			scene:     "review",
			input:     "帮我审合同",
			wantCode:  port.CodeInvalidArgument,
			wantValid: false,
		},
		{
			name:      "输入为空",
			scene:     consultScene,
			input:     "",
			wantCode:  port.CodeInvalidArgument,
			wantValid: false,
		},
		{
			name:      "输入全是空白",
			scene:     consultScene,
			input:     "   \n\t",
			wantCode:  port.CodeInvalidArgument,
			wantValid: false,
		},
		{
			name:      "输入超长",
			scene:     consultScene,
			input:     strings.Repeat("字", maxConsultInputRunes+1),
			wantCode:  port.CodeInvalidArgument,
			wantValid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateInput(tt.scene, tt.input)
			if got := port.CodeOf(err); got != tt.wantCode {
				t.Errorf("got code %d, want %d", got, tt.wantCode)
			}
			if valid := err == nil; valid != tt.wantValid {
				t.Errorf("valid = %v, want %v", valid, tt.wantValid)
			}
		})
	}
}
