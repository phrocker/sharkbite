#include "data/constructs/rfile/RFileOperations.h"
#include "data/streaming/input/HdfsInputStream.h"

namespace cclient{
namespace data{



cclient::data::RFile *RFileOperations::open(const std::string &rfile){

    auto ifs = std::make_unique<std::ifstream >(rfile.c_str(), std::ifstream::binary | std::ifstream::in);

    cclient::data::streams::InputStream *stream = new cclient::data::streams::InputStream(std::move(ifs),0);

    stream = new cclient::data::streams::EndianInputStream(stream);

    std::fstream::pos_type size = filesize(rfile.c_str());

    return new cclient::data::RFile (stream, size);
}

cclient::data::SequentialRFile *RFileOperations::openSequential(const std::string &rfile){
    size_t size = 0;
    std::unique_ptr<cclient::data::streams::InputStream> stream;
    if (rfile.find("hdfs://") != std::string::npos) {
    auto str = std::make_unique<cclient::data::streams::HdfsInputStream>(rfile);
    size = str->getFileSize();
    stream = std::move(str);
    } else {
    size = filesize(rfile.c_str());
    auto in = std::make_unique<std::ifstream>(rfile, std::ifstream::ate | std::ifstream::binary);

    stream = std::make_unique<cclient::data::streams::InputStream>(std::move(in), 0);
    }

    auto endstream = std::make_unique<cclient::data::streams::ReadAheadInputStream>(std::move(stream), 128 * 1024, 1024 * 1024, size);

    return new cclient::data::SequentialRFile (std::move(endstream), size);
}

std::shared_ptr<cclient::data::streams::KeyValueIterator> RFileOperations::openManySequential(const std::vector<std::string> &rfiles){
    std::vector<std::shared_ptr<cclient::data::streams::KeyValueIterator>> iters;
    for (const auto &path : rfiles) {
        size_t size = 0;
        std::unique_ptr<cclient::data::streams::InputStream> stream;
        if (path.find("hdfs://") != std::string::npos) {
        auto str = std::make_unique<cclient::data::streams::HdfsInputStream>(path);
        size = str->getFileSize();
        stream = std::move(str);
        } else {
        size = filesize(path.c_str());
        auto in = std::make_unique<std::ifstream>(path, std::ifstream::ate | std::ifstream::binary);

        stream = std::make_unique<cclient::data::streams::InputStream>(std::move(in), 0);
        }

        auto endstream = std::make_unique<cclient::data::streams::ReadAheadInputStream>(std::move(stream), 128 * 1024, 1024 * 1024, size);

        if (rfiles.size() == 1) {
        return std::move(std::make_unique<cclient::data::SequentialRFile>(std::move(endstream), size));
        } else {
        auto newRFile = std::make_shared<cclient::data::SequentialRFile>(std::move(endstream), size);
        iters.emplace_back(newRFile);
        }
    }
    return std::move(std::make_shared<cclient::data::MultiIterator>(iters));
}

std::ifstream::pos_type RFileOperations::filesize(const char* filename)
    {
        std::ifstream in(filename, std::ifstream::ate | std::ifstream::binary);
        return in.tellg();
    }


}
}
