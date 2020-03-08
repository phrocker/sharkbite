#include "data/constructs/rfile/RFileOperations.h"

namespace cclient{
namespace data{

RFile *RFileOperations::open(const std::string &file){
    

    auto ifs = std::make_unique<std::ifstream>( file.c_str(), std::ifstream::binary | std::ifstream::in);

    cclient::data::streams::InputStream *stream = new cclient::data::streams::InputStream(ifs,0);

    stream = new cclient::data::streams::EndianInputStream(stream);


    std::fstream::pos_type size = filesize(file.c_str());
    return new cclient::data::RFile (stream, size);
}

}

}