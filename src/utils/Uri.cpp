/*
 * Copyright (c) Facebook, Inc. and its affiliates.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#include "utils/Uri.h"

#include <algorithm>
#include <cctype>
#include <regex>

namespace utils {

std::string Uri::smatchBuild(const std::smatch& m, int idx) {
  const auto& sub = m[idx];
  return std::string(sub.first, sub.second);
}

Uri::Uri(std::string str) : hasAuthority_(false), port_(0) {
  static const std::regex uriRegex(
      "([a-zA-Z][a-zA-Z0-9+.-]*):"  // scheme:
      "([^?#]*)"                    // authority and path
      "(?:\\?([^#]*))?"             // ?query
      "(?:#(.*))?");                // #fragment
  static const std::regex authorityAndPathRegex("//([^/]*)(/.*)?");

  std::smatch match;
  if (SH_UNLIKELY(!std::regex_match(str, match, uriRegex))) {
    throw std::runtime_error("invalid URI ");
  }

  scheme_ = smatchBuild(match, 1);
  std::transform(scheme_.begin(), scheme_.end(), scheme_.begin(), ::tolower);

  std::string authorityAndPath(match[2].first, match[2].second);
  std::smatch authorityAndPathMatch;
  if (!std::regex_match(authorityAndPath, authorityAndPathMatch,
                        authorityAndPathRegex)) {
    // Does not start with //, doesn't have authority
    hasAuthority_ = false;
    path_ = authorityAndPath;
  } else {
    static const std::regex authorityRegex(
        "(?:([^@:]*)(?::([^@]*))?@)?"  // username, password
        "(\\[[^\\]]*\\]|[^\\[:]*)"     // host (IP-literal (e.g. '['+IPv6+']',
                                       // dotted-IPv4, or named host)
        "(?::(\\d*))?");               // port

    const auto authority = authorityAndPathMatch[1];
    std::smatch authorityMatch;
    if (!std::regex_match(authority.first, authority.second, authorityMatch,
                          authorityRegex)) {
      throw std::invalid_argument("invalid URI authority ");
    }

    std::string port(authorityMatch[4].first, authorityMatch[4].second);
    if (!port.empty()) {
      port_ = atoi(port.c_str());
    }

    hasAuthority_ = true;
    username_ = smatchBuild(authorityMatch, 1);
    password_ = smatchBuild(authorityMatch, 2);
    host_ = smatchBuild(authorityMatch, 3);
    path_ = smatchBuild(authorityAndPathMatch, 2);
  }

  query_ = smatchBuild(match, 3);
  fragment_ = smatchBuild(match, 4);
}

}  // namespace utils