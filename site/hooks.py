# Copyright 2026 The A2A Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""MkDocs hooks: versions the extra CSS URL by its content, so browsers
fetch it again after every change, and drops the table of contents of pages
that hide it, which the integrated navigation would otherwise still show."""

import hashlib
import os

_CSS = "css/extra.css"
_version = ""


def on_config(config):
    global _version
    path = os.path.join(os.path.dirname(config.config_file_path), "overrides", _CSS)
    with open(path, "rb") as f:
        _version = hashlib.sha256(f.read()).hexdigest()[:10]


def on_page_context(context, page, config, nav):
    if "toc" in page.meta.get("hide", []):
        page.toc.items = []
    return context


def on_post_page(output, page, config):
    return output.replace(f'{_CSS}"', f'{_CSS}?v={_version}"')
