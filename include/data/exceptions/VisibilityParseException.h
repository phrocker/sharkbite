/*
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

#ifndef VISIBILITY_PARSE_EXCEPTION_H
#define VISIBILITY_PARSE_EXCEPTION_H

#include <string>

namespace cclient {
namespace exceptions {

class VisibilityParseException : public std::exception {
 public:
  explicit VisibilityParseException(std::string message,
                                    const std::string& expr, int32_t offset)
      : excp_str(std::move(message)), terms(expr), offset(offset) {}

  ~VisibilityParseException() throw() {}
  const char* what() { return excp_str.c_str(); }

 private:
  std::string excp_str;
  std::string terms;
  int32_t offset;
};
}  // namespace exceptions
} /* namespace cclient */
#endif /* CLIENTEXCEPTION_H_ */
