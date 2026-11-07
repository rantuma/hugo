// Copyright 2026 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mcpserver

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
)

// A code block that does not fit in the current part starts in the next part instead of breaking its first line.
func TestPaginateKeepsLinesWhole(t *testing.T) {
	c := qt.New(t)
	intro, line := strings.Repeat("a", 70), strings.Repeat("b", 40)
	code := "```go\n" + line + "\n" + line + "\n" + line + "\n```"
	parts, unitParts := paginate([]unit{{markdown: intro}, {markdown: code}}, 100)
	c.Assert(parts, qt.DeepEquals, []string{
		intro,
		"```go\n" + line + "\n" + line + "\n```",
		"```go\n" + line + "\n```",
	})
	c.Assert(unitParts, qt.DeepEquals, []int{1, 2})
}
