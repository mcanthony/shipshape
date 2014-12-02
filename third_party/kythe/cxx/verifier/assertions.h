/*
 * Copyright 2014 Google Inc. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#ifndef KYTHE_CXX_VERIFIER_ASSERTIONS_H_
#define KYTHE_CXX_VERIFIER_ASSERTIONS_H_

#include <unordered_map>
#include <vector>

#include "assertions.tab.hh"
#include "assertion_ast.h"

// Required by generated code.
#define YY_DECL                                                               \
  yy::AssertionParserImpl::symbol_type kythe::verifier::AssertionParser::lex( \
      ::kythe::verifier::AssertionParser &context)

namespace kythe {
namespace verifier {

class Verifier;

/// \brief Parses logic programs.
///
/// `AssertionParser` collects together all goals and data that are part of
/// a verification program. This program is then combined with a database of
/// facts (which are merely terms represented in a different, perhaps indexed,
/// format) by the `Verifier`.
class AssertionParser {
 public:
  /// \param trace_lex Dump lexing debug information
  /// \param trace_parse Dump parsing debug information
  explicit AssertionParser(Verifier *verifier, bool trace_lex = false,
                           bool trace_parse = false);

  /// \brief Loads a file containing rules in magic comments.
  /// \param filename The filename of the file to load
  /// \param comment_prefix Lines starting with this prefix are magic (eg "//-")
  /// \return true if there were no errors
  bool ParseInlineRuleFile(const std::string &filename,
                           const char *comment_prefix);

  /// \brief Loads a string containing rules in magic comments.
  /// \param content The content to parse and load
  /// \param fake_filename Some string to use when printing errors and locations
  /// \param comment_prefix Lines starting with this prefix are magic (eg "//-")
  /// \return true if there were no errors
  bool ParseInlineRuleString(const std::string &content,
                             const std::string &fake_filename,
                             const char *comment_prefix);

  /// \brief The name of the current file being read.
  std::string &file() { return file_; }

  /// \brief This `AssertionParser`'s associated `Verifier`.
  Verifier &verifier() { return verifier_; }

  /// \brief All of the goals in this `AssertionParser`, implicitly conjoined.
  std::vector<AstNode *> &goals() { return goals_; }

  /// \brief All of the inspections in this `AssertionParser`.
  std::vector<std::pair<std::string, EVar *>> &inspections() {
    return inspections_;
  }

  /// \brief Unescapes a string literal (which is expected to include
  /// terminating quotes).
  /// \param yytext literal string to escape
  /// \param out pointer to a string to overwrite with `yytext` unescaped.
  /// \return true if `yytext` was a valid literal string; false otherwise.
  static bool Unescape(const char *yytext, std::string *out);

 private:
  friend class yy::AssertionParserImpl;

  /// \brief Resets the magic comment token check.
  /// \sa NextLexCheck
  void ResetLexCheck();

  /// \brief Advances the magic comment token check.
  ///
  /// It isn't possible to bake magic comments into the lexer because there is
  /// not a single supported comment syntax across all languages; while many
  /// do allow BCPL-style // comments, some (like Python) do not. The lexer
  /// starts each line by calling `NextLexCheck` on each character until
  /// it determines whether the line begins with a magic comment or not.
  /// Whitespace (\t ) is ignored.
  ///
  /// \param yytext A 1-length string containing the character to check.
  /// \return 0 on inconclusive; 1 if this is a magic comment; -1 if this is
  /// an ordinary source line.
  int NextLexCheck(const char *yytext);

  /// \brief Records source text after determining that it does not
  /// begin with a magic comment marker.
  /// \param yytext A 1-length string containing the character to append.
  /// \sa NextLexCheck
  void AppendToLine(const char *yytext);

  /// \brief Called at the end of an ordinary line of source text to resolve
  /// available forward location references.
  ///
  /// Certain syntactic features (like `@'token`) refer to elements on the
  /// next line of source text. After that next line is buffered using
  /// `AppendToLine`, the lexer calls to `ResolveLocations` to point those
  /// features at the correct locations.
  ///
  /// \return true if all locations could be resolved
  bool ResolveLocations(const yy::location &end_of_line,
                        size_t offset_after_endline);

  /// \brief Called by the lexer to save the end location of the current file
  /// or buffer.
  void save_eof(const yy::location &eof, size_t eof_ofs) {
    last_eof_ = eof;
    last_eof_ofs_ = eof_ofs;
  }

  /// \note Implemented by generated code care of flex.
  static yy::AssertionParserImpl::symbol_type lex(AssertionParser &context);

  /// \brief Used by the lexer and parser to report errors.
  /// \param location Source location where an error occurred.
  /// \param message Text of the error.
  void Error(const yy::location &location, const std::string &message);

  /// \brief Used by the lexer and parser to report errors.
  /// \param message Text of the error.
  void Error(const std::string &message);

  /// \brief Initializes the lexer to scan from file_.
  /// \note Implemented in `Assertions.ll`.
  void ScanBeginFile(bool trace_scanning);

  /// \brief Initializes the lexer to scan from a string.
  /// \note Implemented in `Assertions.ll`.
  void ScanBeginString(const std::string &data, bool trace_scanning);

  /// \brief Handles end-of-scan actions and destroys any buffers.
  /// \note Implemented in `Assertions.ll`.
  void ScanEnd(const yy::location &eof_loc, size_t eof_loc_ofs);
  AstNode **PopNodes(size_t node_count);
  void PushNode(AstNode *node);
  void AppendGoal(AstNode *goal);

  /// \brief Generates deduplicated `Identifier`s or `EVar`s.
  /// \param location Source location of the token.
  /// \param for_token Token to check.
  /// \return An `EVar` if `for_token` starts with a capital letter;
  /// an `Identifier` otherwise.
  /// \sa CreateEVar, CreateIdentifier
  AstNode *CreateAtom(const yy::location &location,
                      const std::string &for_token);


  /// \brief Generates an equality constraint between the lhs and the rhs.
  /// \param location Source location of the "=" token.
  /// \param lhs The lhs of the equality.
  /// \param rhs The rhs of the equality.
  AstNode *CreateEqualityConstraint(const yy::location &location, AstNode *lhs,
                                    AstNode *rhs);

  /// \brief Generates deduplicated `EVar`s.
  /// \param location Source location of the token.
  /// \param for_token Token to use.
  /// \return A new `EVar` if `for_token` has not yet been made into
  /// an `EVar` already, or the previous `EVar` returned the last
  /// time `CreateEVar` was called.
  EVar *CreateEVar(const yy::location &location, const std::string &for_token);

  /// \brief Generates deduplicated `Identifier`s.
  /// \param location Source location of the text.
  /// \param for_text text to use.
  /// \return A new `Identifier` if `for_text` has not yet been made into
  /// an `Identifier` already, or the previous `Identifier` returned the last
  /// time `CreateIdenfier` was called.
  Identifier *CreateIdentifier(const yy::location &location,
                               const std::string &for_text);

  /// \brief Creates an anonymous `EVar` to implement the `_` token.
  /// \param location Source location of the token.
  AstNode *CreateDontCare(const yy::location &location);

  /// \brief Adds an inspect post-action to the current goal.
  /// \param location Source location for the inspection.
  /// \param for_exp Expression to inspect.
  /// \return An inspection record.
  AstNode *CreateInspect(const yy::location &location,
                         const std::string &inspect_id, AstNode *to_inspect);

  void PushLocationSpec(const std::string &for_token);

  AstNode *CreateAnchorSpec(const yy::location &location);

  AstNode *CreateSimpleEdgeFact(const yy::location &location, AstNode *edge_lhs,
                                const std::string &literal_kind,
                                AstNode *edge_rhs, AstNode *ordinal);

  AstNode *CreateSimpleNodeFact(const yy::location &location, AstNode *lhs,
                                const std::string &literal_key, AstNode *value);

  Identifier *PathIdentifierFor(const yy::location &location,
                                const std::string &path_fragment,
                                const std::string &default_root);

  Verifier &verifier_;

  /// The arena from the verifier; needed by the parser implementation.
  Arena *arena_;

  std::vector<AstNode *> goals_;
  std::vector<std::pair<EVar *, std::string>> unresolved_locations_;
  std::vector<AstNode *> node_stack_;
  std::vector<std::string> location_spec_stack_;
  std::string file_;
  std::string line_;
  /// The comment prefix we're looking for.
  std::string lex_check_against_;
  /// How many characters into lex_check_against_ we've seen.
  int lex_check_buffer_size_;
  /// Did we encounter errors during lexing or parsing?
  bool had_errors_ = false;
  /// Save the end-of-file location from the lexer.
  yy::location last_eof_;
  size_t last_eof_ofs_;
  /// Inspections to be performed after all goals have been solved.
  std::vector<std::pair<std::string, EVar *>> inspections_;
  /// Context mapping symbols to AST nodes.
  std::unordered_map<Symbol, Identifier *> identifier_context_;
  std::unordered_map<Symbol, EVar *> evar_context_;
  /// Are we dumping lexer trace information?
  bool trace_lex_;
  /// Are we dumping parser trace information?
  bool trace_parse_;
};

}  // namespace verifier
}  // namespace kythe

#endif  // KYTHE_CXX_VERIFIER_ASSERTIONS_H_