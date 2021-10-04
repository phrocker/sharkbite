/**
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
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
#include <functional>
#include <iostream>
#ifdef WIN32
#include <cctype>
#include <cwctype>
#endif
#include <algorithm>
#include <map>
#include <sstream>
#include <vector>

enum TimeUnit { DAY, HOUR, MINUTE, SECOND, MILLISECOND, NANOSECOND };
#if defined(WIN32) || (__cplusplus >= 201103L &&                             \
                       (!defined(__GLIBCXX__) || (__cplusplus >= 201402L) || \
                        (defined(_GLIBCXX_RELEASE) && _GLIBCXX_RELEASE > 4)))
#define HAVE_REGEX_CPP 1
#else
#define HAVE_REGEX_CPP 0
#endif

namespace utils {

namespace {
template <class Char>
struct string_traits;
template <>
struct string_traits<char> {
  template <class T>
  static std::string convert_to_string(T&& t) {
    return std::to_string(std::forward<T>(t));
  }
};

template <>
struct string_traits<wchar_t> {
  template <class T>
  static std::wstring convert_to_string(T&& t) {
    return std::to_wstring(std::forward<T>(t));
  }
};
}  // namespace

/**
 * Stateless String utility class.
 *
 * Design: Static class, with no member variables
 *
 * Purpose: Houses many useful string utilities.
 */
class StringUtils {
 public:
  /**
   * Converts a string to a boolean
   * Better handles mixed case.
   * @param input input string
   * @param output output string.
   */
  static bool StringToBool(std::string input, bool& output);

  // Trim String utils

  /**
   * Trims a string left to right
   * @param s incoming string
   * @returns modified string
   */
  static std::string trim(std::string s);

  /**
   * Trims left most part of a string
   * @param s incoming string
   * @returns modified string
   */
  static inline std::string trimLeft(std::string s) {
    s.erase(s.begin(),
            std::find_if(
                s.begin(), s.end(),
                std::not1(std::pointer_to_unary_function<int, int>(isspace))));
    return s;
  }

  /**
   * Trims a string on the right
   * @param s incoming string
   * @returns modified string
   */

  static inline std::string trimRight(std::string s) {
    s.erase(std::find_if(
                s.rbegin(), s.rend(),
                std::not1(std::pointer_to_unary_function<int, int>(isspace)))
                .base(),
            s.end());
    return s;
  }

  /**
   * Compares strings by lower casing them.
   */
  static inline bool equalsIgnoreCase(const std::string& left,
                                      const std::string& right) {
    if (left.length() == right.length()) {
      return std::equal(right.begin(), right.end(), left.begin(),
                        [](unsigned char lc, unsigned char rc) {
                          return tolower(lc) == tolower(rc);
                        });
    } else {
      return false;
    }
  }

  static std::vector<std::string> split(const std::string& str,
                                        const std::string& delimiter);

  /**
   * Converts a string to a float
   * @param input input string
   * @param output output float
   * @param cp failure policy
   */
  static bool StringToFloat(std::string input, float& output);

  static std::string replaceEnvironmentVariables(std::string& original_string);

  static std::string& replaceAll(std::string& source_string,
                                 const std::string& from_string,
                                 const std::string& to_string);

  inline static bool endsWithIgnoreCase(const std::string& value,
                                        const std::string& endString) {
    if (endString.size() > value.size()) return false;
    return std::equal(endString.rbegin(), endString.rend(), value.rbegin(),
                      [](unsigned char lc, unsigned char rc) {
                        return tolower(lc) == tolower(rc);
                      });
  }

  inline static bool endsWith(const std::string& value,
                              const std::string& endString) {
    if (endString.size() > value.size()) return false;
    return std::equal(endString.rbegin(), endString.rend(), value.rbegin());
  }

  inline static std::string hex_ascii(const std::string& in) {
    int len = in.length();
    std::string newString;
    for (int i = 0; i < len; i += 2) {
      std::string sstr = in.substr(i, 2);
      char chr = (char)(int)strtol(sstr.c_str(), 0x00, 16);
      newString.push_back(chr);
    }
    return newString;
  }

  /**
   * Concatenates strings stored in an arbitrary container using the provided
   * separator.
   * @tparam TChar char type of the string (char or wchar_t)
   * @tparam U arbitrary container which has string or wstring value type
   * @param separator that is inserted between each elements. Type should match
   * the type of strings in container.
   * @param container that contains the strings to be concatenated
   * @return the result string
   */
  template <class TChar, class U,
            typename std::enable_if<
                std::is_same<typename U::value_type,
                             std::basic_string<TChar>>::value>::type* = nullptr>
  static std::basic_string<TChar> join(
      const std::basic_string<TChar>& separator, const U& container) {
    typedef typename U::const_iterator ITtype;
    ITtype it = container.cbegin();
    std::basic_stringstream<TChar> sstream;
    while (it != container.cend()) {
      sstream << (*it);
      ++it;
      if (it != container.cend()) {
        sstream << separator;
      }
    }
    return sstream.str();
  };

  /**
   * Just a wrapper for the above function to be able to create separator from
   * const char* or const wchar_t*
   */
  template <class TChar, class U,
            typename std::enable_if<
                std::is_same<typename U::value_type,
                             std::basic_string<TChar>>::value>::type* = nullptr>
  static std::basic_string<TChar> join(const TChar* separator,
                                       const U& container) {
    return join(std::basic_string<TChar>(separator), container);
  };

  /**
   * Concatenates string representation of integrals stored in an arbitrary
   * container using the provided separator.
   * @tparam TChar char type of the string (char or wchar_t)
   * @tparam U arbitrary container which has any integral value type
   * @param separator that is inserted between each elements. Type of this
   * determines the result type. (wstring separator -> wstring)
   * @param container that contains the integrals to be concatenated
   * @return the result string
   */
  template <
      class TChar, class U,
      typename std::enable_if<
          std::is_integral<typename U::value_type>::value>::type* = nullptr,
      typename std::enable_if<
          !std::is_same<U, std::basic_string<TChar>>::value>::type* = nullptr>
  static std::basic_string<TChar> join(
      const std::basic_string<TChar>& separator, const U& container) {
    typedef typename U::const_iterator ITtype;
    ITtype it = container.cbegin();
    std::basic_stringstream<TChar> sstream;
    while (it != container.cend()) {
      sstream << string_traits<TChar>::convert_to_string(*it);
      ++it;
      if (it != container.cend()) {
        sstream << separator;
      }
    }
    return sstream.str();
  }

  /**
   * Just a wrapper for the above function to be able to create separator from
   * const char* or const wchar_t*
   */
  template <
      class TChar, class U,
      typename std::enable_if<
          std::is_integral<typename U::value_type>::value>::type* = nullptr,
      typename std::enable_if<
          !std::is_same<U, std::basic_string<TChar>>::value>::type* = nullptr>
  static std::basic_string<TChar> join(const TChar* separator,
                                       const U& container) {
    return join(std::basic_string<TChar>(separator), container);
  }
  static std::string replaceMap(
      std::string source_string,
      const std::map<std::string, std::string>& replace_map);

  static std::string getEscapedString(std::string const& s);
};

} /* namespace utils */
