/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"gotest.tools/assert"
	"gotest.tools/v3/icmd"

	. "github.com/docker/compose-cli/tests/framework"
)

func TestLocalBackendComposeUp(t *testing.T) {
	c := NewParallelE2eCLI(t, binDir)
	c.RunDockerCmd("context", "create", "local", "test-context").Assert(t, icmd.Success)
	c.RunDockerCmd("context", "use", "test-context").Assert(t, icmd.Success)

	const projectName = "compose-e2e-demo"

	t.Run("up", func(t *testing.T) {
		c.RunDockerCmd("compose", "up", "-f", "../../tests/composefiles/demo_multi_port.yaml", "--project-name", projectName)
		t.Cleanup(func() {
			_ = c.RunDockerCmd("compose", "down", "--project-name", projectName)
		})
		res := c.RunDockerCmd("compose", "ps", "-p", projectName)
		res.Assert(t, icmd.Expected{Out: `web`})

		endpoint := "http://localhost:80"
		output := HTTPGetWithRetry(t, endpoint+"/words/noun", http.StatusOK, 2*time.Second, 20*time.Second)
		assert.Assert(t, strings.Contains(output, `"word":`))
	})
}
