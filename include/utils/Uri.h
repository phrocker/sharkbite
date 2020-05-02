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

#pragma once
#include <vector>
#include <regex>
#include <string>

namespace utils{

#ifndef SH_UNLIKELY
#ifdef __GNUC__
#define SH_UNLIKELY(val) (__builtin_expect((val), 0))
#define SH_LIKELY(val) (__builtin_expect((val), 1))
#else
    #define SH_UNLIKELY(val) (val)
    #define SH_LIKELY(val) (val)
    #endif
#endif

class Uri {
 public:

  explicit Uri(std::string str);

    std::string scheme() const {
    return scheme_;
  }
  const std::string username() const {
    return username_;
  }
  const std::string password() const {
    return password_;
  }

  std::string smatchBuild(const std::smatch& m, int idx);
  
  std::string host() const {
    return host_;
  }
 
  std::string hostname() const;
  uint16_t 
  port() const {
    return port_;
  }
  std::string path() const {
    return path_;
  }
  std::string query() const {
    return query_;
  }
  std::string fragment() const {
    return fragment_;
  }

  std::string authority() const;


  void setPort(uint16_t port) {
    hasAuthority_ = true;
    port_ = port;
  }

 private:
  std::string scheme_;
  std::string username_;
  std::string password_;
  std::string host_;
  bool hasAuthority_;
  uint16_t port_;
  std::string path_;
  std::string query_;
  std::string fragment_;
  std::vector<std::pair<std::string, std::string>> queryParams_;
};
}